package priors

import (
	"fmt"
	"strings"
)

// LaunchParameter is one launch argument.
//
// IT IS THE ARGUMENT AND ITS VALUE, because an engine accepts a flag with
// its value attached and a flag with its value in the next argument, and both
// spellings describe the same launch.
type LaunchParameter struct {
	// Flag is the argument name, with its leading dashes.
	Flag string

	// Value is the argument value, empty for a bare flag.
	Value string
}

// ParseLaunchParameters reads an ordered argument list.
//
// IT READS BOTH SPELLINGS, because an engine accepts a flag with its value
// attached and a flag with its value in the next argument.
func ParseLaunchParameters(tokens []string) ([]LaunchParameter, error) {
	parameters := make([]LaunchParameter, 0, len(tokens))
	for i := 0; i < len(tokens); i++ {
		token := tokens[i]
		if token == "--" {
			return nil, fmt.Errorf("the arguments carry a %q separator", token)
		}
		flag, value, inline := strings.Cut(token, "=")
		if inline {
			if value == "" {
				return nil, fmt.Errorf("the argument %q carries no value", token)
			}
			parameters = append(parameters, LaunchParameter{Flag: flag, Value: value})
			continue
		}
		if i+1 < len(tokens) && !strings.HasPrefix(tokens[i+1], "-") {
			value = tokens[i+1]
			i++
		}
		parameters = append(parameters, LaunchParameter{Flag: flag, Value: value})
	}
	return parameters, nil
}

// RenderLaunchParameters renders arguments into the ordered token list a
// command line carries.
//
// IT RENDERS A VALUE IN ITS OWN TOKEN, because that is the spelling the
// measured launch used and a deployment declares its arguments the same way.
func RenderLaunchParameters(parameters []LaunchParameter) []string {
	tokens := make([]string, 0, len(parameters)*2)
	for _, parameter := range parameters {
		tokens = append(tokens, parameter.Flag)
		if parameter.Value != "" {
			tokens = append(tokens, parameter.Value)
		}
	}
	return tokens
}

// CheckEffectiveLaunchParameters checks that the effective launch parameters
// describe the same launch as the launch parameters.
//
// IT REFUSES WHEN THEY DO NOT, because an effective list that names other
// arguments describes a different launch.
//
// IT IS EMPTY WHEN THERE IS NO EFFECTIVE LIST, because a launch that ran with
// the arguments it was given has nothing to record.
func CheckEffectiveLaunchParameters(launch, effective []string) error {
	if len(effective) == 0 {
		return nil
	}

	launchParameters, err := ParseLaunchParameters(launch)
	if err != nil {
		return fmt.Errorf("the launch arguments cannot be read: %w", err)
	}
	effectiveParameters, err := ParseLaunchParameters(effective)
	if err != nil {
		return fmt.Errorf("the effective launch arguments cannot be read: %w", err)
	}
	if len(effectiveParameters) != len(launchParameters) {
		return fmt.Errorf(
			"the effective arguments name %d arguments and the launch arguments name %d",
			len(effectiveParameters),
			len(launchParameters),
		)
	}
	for i := range launchParameters {
		if launchParameters[i].Flag != effectiveParameters[i].Flag {
			return fmt.Errorf(
				"the effective argument %q is not the launch argument %q",
				effectiveParameters[i].Flag,
				launchParameters[i].Flag,
			)
		}
	}
	return nil
}
