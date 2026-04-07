package setting

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	conregname "github.com/google/go-containerregistry/pkg/name"
	cron "github.com/robfig/cron/v3"

	"gpustack.ai/gpustack/pkg/utils/osx"
	"gpustack.ai/gpustack/pkg/utils/stringx"
)

// Initializer defines the stereotype for initializing the default value.
type Initializer func(name string) (value string)

// InitializeFromSpecifiedEnv searches the variable of the given environment name,
// returns the default value if not found the specified environment.
func InitializeFromSpecifiedEnv(envName string, defValue ...string) Initializer {
	return func(_ string) string {
		return osx.ExpandEnv(envName, defValue...)
	}
}

// InitializeFromEnv searches the variable of the `GPUSTACK_${UpperSnakeCase_of_SettingName}`,
// returns the default value if not found the environment.
func InitializeFromEnv(defValue ...string) Initializer {
	return func(name string) string {
		envName := "GPUSTACK_" + stringx.UnderscoreUpper(name)
		return osx.ExpandEnv(envName, defValue...)
	}
}

// InitializeFrom initializes with the given value.
func InitializeFrom(value string) Initializer {
	return func(_ string) string {
		return value
	}
}

// Admission defines the stereotype for validating writing.
type Admission func(ctx context.Context, oldVal, newVal string) (err error)

// ErrAdmissionSkipped is the error returned by Admission to indicate the validation is skipped.
var ErrAdmissionSkipped = errors.New("admission skipped")

// AdmitWith combines multiple Admission into one,
// if no Admission given, it will be Allow.
//
// If one Admission of the given Admissions returns an error,
// the error will be returned immediately.
//
// If one Admission of the given Admissions returns an ErrAdmissionSkipped,
// the function will return true immediately.
func AdmitWith(admits ...Admission) Admission {
	if len(admits) == 0 {
		admits = []Admission{Allow()}
	}
	return func(ctx context.Context, oldValue, newValue string) error {
		var err error
		for i := range admits {
			err = admits[i](ctx, oldValue, newValue)
			if err != nil {
				break
			}
		}
		if errors.Is(err, ErrAdmissionSkipped) {
			return nil
		}
		return err
	}
}

// Allow implements the Admission stereotype,
// which means the value can be modified.
func Allow() Admission {
	return func(ctx context.Context, oldVal, newVal string) error {
		return nil
	}
}

// Disallow implements the Admission stereotype,
// which means the value can not be modified.
func Disallow() Admission {
	return func(ctx context.Context, oldVal, newVal string) error {
		return errors.New("cannot modify")
	}
}

// AllowBlank implements the Admission stereotype,
// which means the value can be modified if blank.
//
// AllowBlank always combines with other Admission,
// if allow input anything, please use Allow instead.
func AllowBlank() Admission {
	return func(ctx context.Context, oldVal, newVal string) error {
		if !isBlank(newVal) {
			return nil
		}
		return ErrAdmissionSkipped
	}
}

// DisallowBlank implements the Admission stereotype,
// which means the value can be modified if not blank.
//
// DisallowBlank always combines with other Admission,
// if disallow input anything, please use Disallow instead.
func DisallowBlank() Admission {
	return func(ctx context.Context, oldVal, newVal string) error {
		if !isBlank(newVal) {
			return nil
		}
		return errors.New("blank value")
	}
}

// AllowOnceConfigure implements the Admission stereotype,
// which means the value can be modified if blank.
func AllowOnceConfigure() Admission {
	return func(ctx context.Context, oldVal, newVal string) error {
		if isBlank(oldVal) {
			return nil
		}
		return errors.New("already configured")
	}
}

// AllowBool implements the Admission stereotype,
// which means the value can be modified if it's boolean.
func AllowBool() Admission {
	return func(ctx context.Context, oldVal, newVal string) error {
		_, err := strconv.ParseBool(newVal)
		return err
	}
}

// AllowInt64 implements the Admission stereotype,
// which means the value can be modified if it's int64.
func AllowInt64() Admission {
	return func(ctx context.Context, oldVal, newVal string) error {
		_, err := strconv.ParseInt(newVal, 10, 64)
		return err
	}
}

// AllowInt64InRange implements the Admission stereotype,
// which means the value can be modified if it's int64 and in the given range.
func AllowInt64InRange(minVal, maxVal int64) Admission {
	return func(ctx context.Context, oldVal, newVal string) error {
		v, err := strconv.ParseInt(newVal, 10, 64)
		if err != nil {
			return err
		}
		if v < minVal || v > maxVal {
			return fmt.Errorf("value %d is out of range [%d, %d]", v, minVal, maxVal)
		}
		return nil
	}
}

// AllowUint64 implements the Admission stereotype,
// which means the value can be modified if it's uint64.
func AllowUint64() Admission {
	return func(ctx context.Context, oldVal, newVal string) error {
		_, err := strconv.ParseUint(newVal, 10, 64)
		return err
	}
}

// AllowUint64InRange implements the Admission stereotype,
// which means the value can be modified if it's uint64 and in the given range.
func AllowUint64InRange(minVal, maxVal uint64) Admission {
	return func(ctx context.Context, oldVal, newVal string) error {
		v, err := strconv.ParseUint(newVal, 10, 64)
		if err != nil {
			return err
		}
		if v < minVal || v > maxVal {
			return fmt.Errorf("value %d is out of range [%d, %d]", v, minVal, maxVal)
		}
		return nil
	}
}

// AllowFloat64 implements the Admission stereotype,
// which means the value can be modified if it's float64.
func AllowFloat64() Admission {
	return func(ctx context.Context, oldVal, newVal string) error {
		_, err := strconv.ParseFloat(newVal, 64)
		return err
	}
}

// AllowFloat64InRange implements the Admission stereotype,
// which means the value can be modified if it's float64 and in the given range.
func AllowFloat64InRange(minVal, maxVal float64) Admission {
	return func(ctx context.Context, oldVal, newVal string) error {
		v, err := strconv.ParseFloat(newVal, 64)
		if err != nil {
			return err
		}
		if v < minVal || v > maxVal {
			return fmt.Errorf("value %f is out of range [%f, %f]", v, minVal, maxVal)
		}
		return nil
	}
}

// AllowDuration implements the Admission stereotype,
// which means the value can be modified if it's duration.
func AllowDuration() Admission {
	return func(ctx context.Context, oldVal, newVal string) error {
		_, err := time.ParseDuration(newVal)
		return err
	}
}

// AllowDurationInRange implements the Admission stereotype,
// which means the value can be modified if it's float64 and in the given range.
func AllowDurationInRange(minVal, maxVal time.Duration) Admission {
	return func(ctx context.Context, oldVal, newVal string) error {
		v, err := time.ParseDuration(newVal)
		if err != nil {
			return err
		}
		if v < minVal || v > maxVal {
			return fmt.Errorf("value %v is out of range [%v, %v]", v, minVal, maxVal)
		}
		return nil
	}
}

// AllowUrl implements the Admission stereotype,
// which means the value can be modified if it is a URL.
// This Admission allows blank new value,
// if not allowed, combine with DisallowBlank.
func AllowUrl() Admission {
	return func(ctx context.Context, oldVal, newVal string) error {
		return checkUrl(newVal, func(_ url.URL) error {
			return nil
		})
	}
}

// AllowUrlWithSchema implements the Admission stereotype,
// which means the value can be modified if it is a URL with specified schemas.
// This Admission allows blank new value,
// if not allowed, combine with DisallowBlank.
func AllowUrlWithSchema(schema string, otherSchemas ...string) Admission {
	return func(ctx context.Context, oldVal, newVal string) error {
		return checkUrl(newVal, func(u url.URL) error {
			if u.Scheme == schema {
				return nil
			}
			for _, s := range otherSchemas {
				if u.Scheme == s {
					return nil
				}
			}
			return fmt.Errorf("invalid schema: %q, allowed: %s",
				u.Scheme, strings.Join(append([]string{schema}, otherSchemas...), ", "))
		})
	}
}

// AllowCronExpression implements the Admission stereotype,
// which means the value can be modified if it's cron expression.
// This Admission allows blank new value,
// if not allowed, combine with DisallowBlank.
func AllowCronExpression() Admission {
	return func(ctx context.Context, oldVal, newVal string) error {
		_, err := cron.ParseStandard(newVal)
		return err
	}
}

// AllowCronExpressionAtLeast implements the Admission stereotype,
// which means the value can be modified if it's cron expression and at least the given duration.
// This Admission allows blank new value,
// if not allowed, combine with DisallowBlank.
func AllowCronExpressionAtLeast(d time.Duration) Admission {
	return func(ctx context.Context, oldVal, newVal string) error {
		expr, err := cron.ParseStandard(newVal)
		if err != nil {
			return err
		}
		next := expr.Next(time.Now())
		duration := expr.Next(next).Sub(next)
		if duration < d {
			return fmt.Errorf("cron expression %q is too short, at least %v", newVal, d)
		}
		return nil
	}
}

// AllowContainerRegistry implements the Admission stereotype,
// which means the value can be modified if it's container registry.
func AllowContainerRegistry() Admission {
	return func(ctx context.Context, oldVal, newVal string) error {
		_, err := conregname.NewRepository(newVal)
		return err
	}
}

// AllowContainerImageReference implements the Admission stereotype,
// which means the value can be modified if it's container image reference.
func AllowContainerImageReference() Admission {
	return func(ctx context.Context, oldVal, newVal string) error {
		_, err := conregname.ParseReference(newVal)
		return err
	}
}

func isBlank(s string) bool {
	return slices.Contains([]string{"", "{}", "[]"}, strings.TrimSpace(s))
}

func checkUrl(str string, check func(url.URL) error) error {
	v, err := url.Parse(str)
	if err != nil {
		return fmt.Errorf("%s is illegal URL format: %w", str, err)
	}

	port := v.Port()
	if port != "" {
		p, err := strconv.ParseUint(port, 10, 32)
		if err != nil {
			return fmt.Errorf("parsing given port: %w", err)
		}

		if p > 65535 {
			return fmt.Errorf("given port %d: exceeded upper limit", p)
		}
	}

	return check(*v)
}
