package setting

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	conregname "github.com/google/go-containerregistry/pkg/name"
	cron "github.com/robfig/cron/v3"
	storage "k8s.io/api/storage/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	"gpustack.ai/gpustack/pkg/system"
	"gpustack.ai/gpustack/pkg/utils/osx"
	"gpustack.ai/gpustack/pkg/utils/stringx"
)

// Initializer defines the stereotype for initializing a setting's value and environment metadata.
// declared reports whether envName was present, including when its value was empty.
type Initializer func(name string) (value, envName string, declared bool)

// InitializeFromSpecifiedEnv searches the variable of the given environment name,
// returns the default value if not found, and reports the variable's registration-time presence.
func InitializeFromSpecifiedEnv(envName string, defValue ...string) Initializer {
	return func(_ string) (string, string, bool) {
		value := osx.ExpandEnv(envName, defValue...)
		_, declared := osx.LookupEnv(envName)
		return value, envName, declared
	}
}

// InitializeFromEnv searches the variable of the `GPUSTACK_${UpperSnakeCase_of_SettingName}`,
// returns the default value if not found, and reports the variable's registration-time presence.
func InitializeFromEnv(defValue ...string) Initializer {
	return func(name string) (string, string, bool) {
		envName := "GPUSTACK_" + stringx.UnderscoreUpper(name)
		value := osx.ExpandEnv(envName, defValue...)
		_, declared := osx.LookupEnv(envName)
		return value, envName, declared
	}
}

// InitializeFrom initializes with the given value without an environment source.
func InitializeFrom(value string) Initializer {
	return func(_ string) (string, string, bool) {
		return value, "", false
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

// AllowIPv4CIDRList implements the Admission stereotype,
// which means the value can be modified if it's a comma-separated list of IPv4 CIDRs, blank
// included. IPv6 prefixes are refused at the write rather than dropped at the render, so an
// ipBlock except list never loses an entry to a silent filter.
func AllowIPv4CIDRList() Admission {
	return func(ctx context.Context, oldVal, newVal string) error {
		for _, segment := range strings.Split(newVal, ",") {
			cidr := strings.TrimSpace(segment)
			if cidr == "" {
				continue
			}
			ip, _, err := net.ParseCIDR(cidr)
			if err != nil {
				return fmt.Errorf("invalid CIDR %q: %w", cidr, err)
			}
			if ip.To4() == nil {
				return fmt.Errorf("invalid CIDR %q: only IPv4 prefixes can ride an IPv4 rule's except list", cidr)
			}
		}
		return nil
	}
}

// DisallowListWith implements the Admission stereotype,
// which means the value can not be modified if it's a comma-separated list containing the
// forbidden segment. Comparing trimmed segments keeps a padded or buried name the same case
// here as it is at the parse site.
func DisallowListWith(forbidden string) Admission {
	return func(_ context.Context, _, newVal string) error {
		for _, segment := range strings.Split(newVal, ",") {
			if strings.TrimSpace(segment) == forbidden {
				return fmt.Errorf("segment %q is not allowed in this list", forbidden)
			}
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

// AllowUrlWithHost implements the Admission stereotype,
// which means the value can be modified if it is a URL with a non-empty host.
// This Admission allows blank new value,
// if not allowed, combine with DisallowBlank.
func AllowUrlWithHost() Admission {
	return func(ctx context.Context, oldVal, newVal string) error {
		return checkUrl(newVal, func(u url.URL) error {
			if u.Host == "" {
				return fmt.Errorf("%q is not a URL with a host", newVal)
			}
			return nil
		})
	}
}

// DisallowUrlWithUser implements the Admission stereotype,
// which means the value can be modified if it is a URL without user credentials.
func DisallowUrlWithUser() Admission {
	return func(ctx context.Context, oldVal, newVal string) error {
		return checkUrl(newVal, func(u url.URL) error {
			if u.User != nil {
				return errors.New("URL must not carry credentials")
			}
			return nil
		})
	}
}

// AllowEnum implements the Admission stereotype,
// which means the value can be modified if it matches one of the allowed values.
func AllowEnum(allowed ...string) Admission {
	return func(_ context.Context, _, newVal string) error {
		if slices.Contains(allowed, newVal) {
			return nil
		}
		switch len(allowed) {
		case 0:
			return fmt.Errorf("got %q", newVal)
		case 1:
			return fmt.Errorf("want %s, got %q", allowed[0], newVal)
		case 2:
			return fmt.Errorf("want %s or %s, got %q", allowed[0], allowed[1], newVal)
		default:
			return fmt.Errorf("want %s or %s, got %q",
				strings.Join(allowed[:len(allowed)-1], ", "),
				allowed[len(allowed)-1],
				newVal,
			)
		}
	}
}

// csiDriverExists reports whether the named CSIDriver object exists. It reads the API server,
// not an informer cache, so an edit right after a plugin is installed is not refused for a
// CSIDriver the cache has not seen yet. It is a variable so a test can answer without an API
// server.
var csiDriverExists = func(ctx context.Context, driver string) (bool, error) {
	err := system.LoopbackCtrlAPIReader.Get().Get(ctx, ctrlcli.ObjectKey{Name: driver}, &storage.CSIDriver{})
	switch {
	case err == nil:
		return true, nil
	case kerrors.IsNotFound(err):
		return false, nil
	default:
		return false, err
	}
}

// AllowCSIDriverWhen implements the Admission stereotype,
// which means a value in values can be modified only while the named CSIDriver exists.
func AllowCSIDriverWhen(driver string, values ...string) Admission {
	return func(ctx context.Context, _, newVal string) error {
		if !slices.Contains(values, newVal) {
			return nil
		}
		ok, err := csiDriverExists(ctx, driver)
		if err != nil {
			return fmt.Errorf("check the CSIDriver %s: %w", driver, err)
		}
		if !ok {
			return fmt.Errorf("the CSIDriver %s does not exist", driver)
		}
		return nil
	}
}

// currentValue reads another Setting's stored value by name from the Secret itself, not through the
// Settings read cache: two edits within the cache's thirty seconds would otherwise be checked
// against a value already replaced. A Secret that cannot be read is an error, so the admission
// refuses rather than skip the check. It is a variable so a test can answer without an API server.
var currentValue = func(ctx context.Context, h Settings, name string) (string, error) {
	sec, err := system.LoopbackKubeClient.Get().CoreV1().Secrets(DelegatedSecretNamespace).
		Get(ctx, DelegatedSecretName, meta.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("read setting %s: %w", name, err)
	}
	if v, ok := sec.Data[name]; ok {
		return string(v), nil
	}
	return h[name].DefaultValue(), nil
}

// AllowInt64GreaterThan implements the Admission stereotype,
// which means the int64 value must exceed the stored int64 value of the named setting.
func (h Settings) AllowInt64GreaterThan(other string) Admission {
	return h.allowInt64Against(other, true)
}

// AllowInt64LessThan implements the Admission stereotype,
// which means the int64 value must stay below the stored int64 value of the named setting.
func (h Settings) AllowInt64LessThan(other string) Admission {
	return h.allowInt64Against(other, false)
}

func (h Settings) allowInt64Against(other string, greater bool) Admission {
	return func(ctx context.Context, _, newVal string) error {
		v, err := strconv.ParseInt(newVal, 10, 64)
		if err != nil {
			return err
		}
		raw, err := currentValue(ctx, h, other)
		if err != nil {
			return err
		}
		o, readable := parseInt64(raw)
		switch {
		case !readable:
			// An unreadable peer value is left to its own check.
		case greater && v <= o:
			return fmt.Errorf("value %d must be above %s (%d)", v, other, o)
		case !greater && v >= o:
			return fmt.Errorf("value %d must be below %s (%d)", v, other, o)
		}
		return nil
	}
}

func parseInt64(raw string) (int64, bool) {
	v, err := strconv.ParseInt(raw, 10, 64)
	return v, err == nil
}

// AllowNonNegativeInt64Quantity implements the Admission stereotype,
// which means the value can be modified if it is a non-negative integer resource quantity.
func AllowNonNegativeInt64Quantity() Admission {
	return func(_ context.Context, _, newVal string) error {
		q, err := resource.ParseQuantity(newVal)
		if err != nil {
			return fmt.Errorf("parse quantity %q: %w", newVal, err)
		}
		if q.Sign() < 0 {
			return fmt.Errorf("quantity %q is negative", newVal)
		}
		if _, ok := q.AsInt64(); !ok {
			return fmt.Errorf("quantity %q is not a whole integer", newVal)
		}
		return nil
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
