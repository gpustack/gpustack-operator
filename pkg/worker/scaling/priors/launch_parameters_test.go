package priors

import (
	"slices"
	"testing"
)

// TestParseLaunchParametersReadsBothSpellings asserts an argument list is read
// the way an engine reads it.
//
// IT MATTERS BECAUSE A FLAG WITH ITS VALUE ATTACHED AND A FLAG WITH ITS VALUE
// IN THE NEXT ARGUMENT DESCRIBE THE SAME LAUNCH, and a reader that understood
// only one of them would read a launch as a different one.
func TestParseLaunchParametersReadsBothSpellings(t *testing.T) {
	cases := []struct {
		name   string
		tokens []string
		want   []LaunchParameter
	}{
		{
			name:   "a value in its own argument",
			tokens: []string{"--mem-fraction-static", "0.8"},
			want:   []LaunchParameter{{Flag: "--mem-fraction-static", Value: "0.8"}},
		},
		{
			name:   "a value attached to its flag",
			tokens: []string{"--mem-fraction-static=0.8"},
			want:   []LaunchParameter{{Flag: "--mem-fraction-static", Value: "0.8"}},
		},
		{
			name:   "a bare flag",
			tokens: []string{"--disable-radix-cache"},
			want:   []LaunchParameter{{Flag: "--disable-radix-cache"}},
		},
		{
			name: "a flag and a value that looks like a flag",
			tokens: []string{
				"--disable-radix-cache",
				"--mem-fraction-static", "0.8",
			},
			want: []LaunchParameter{
				{Flag: "--disable-radix-cache"},
				{Flag: "--mem-fraction-static", Value: "0.8"},
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := ParseLaunchParameters(testCase.tokens)
			if err != nil {
				t.Fatalf("parse launch parameters: %v", err)
			}
			if len(got) != len(testCase.want) {
				t.Fatalf("the parser read %d arguments, want %d", len(got), len(testCase.want))
			}
			for i := range got {
				if got[i] != testCase.want[i] {
					t.Fatalf("the parser read %v, want %v", got, testCase.want)
				}
			}
		})
	}
}

// TestParseLaunchParametersRefusesAnEmptyValue asserts an argument that
// carries no value is refused.
func TestParseLaunchParametersRefusesAnEmptyValue(t *testing.T) {
	if _, err := ParseLaunchParameters([]string{"--mem-fraction-static="}); err == nil {
		t.Fatal("expected an empty argument value to be refused")
	}
}

// TestRenderLaunchParametersRoundTrips asserts the rendered arguments read
// back as the arguments they were rendered from.
func TestRenderLaunchParametersRoundTrips(t *testing.T) {
	want := []LaunchParameter{
		{Flag: "--disable-radix-cache"},
		{Flag: "--mem-fraction-static", Value: "0.8"},
	}

	rendered := RenderLaunchParameters(want)
	got, err := ParseLaunchParameters(rendered)
	if err != nil {
		t.Fatalf("parse the rendered arguments: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("the rendered arguments read back as %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("the rendered arguments read back as %v, want %v", got, want)
		}
	}
}

// TestCheckEffectiveLaunchParametersRefusesAnotherLaunch asserts an effective
// list that names other arguments is refused.
func TestCheckEffectiveLaunchParametersRefusesAnotherLaunch(t *testing.T) {
	launch := []string{"--disable-radix-cache", "--mem-fraction-static", "0.8"}

	cases := []struct {
		name      string
		effective []string
	}{
		{
			name:      "another argument",
			effective: []string{"--disable-radix-cache", "--mem-fraction-static", "0.68", "--extra", "1"},
		},
		{
			name:      "another order",
			effective: []string{"--mem-fraction-static", "0.68", "--disable-radix-cache"},
		},
		{
			name:      "fewer arguments",
			effective: []string{"--disable-radix-cache"},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if err := CheckEffectiveLaunchParameters(launch, testCase.effective); err == nil {
				t.Fatal("expected the effective arguments to be refused")
			}
		})
	}
}

// TestCheckEffectiveLaunchParametersAcceptsTheSameLaunch asserts an effective
// list that names the same arguments is accepted, and that an absent list is
// accepted.
func TestCheckEffectiveLaunchParametersAcceptsTheSameLaunch(t *testing.T) {
	launch := []string{"--disable-radix-cache", "--mem-fraction-static", "0.8"}
	effective := []string{"--disable-radix-cache", "--mem-fraction-static", "0.68"}

	if err := CheckEffectiveLaunchParameters(launch, effective); err != nil {
		t.Fatalf("the effective arguments were refused: %v", err)
	}
	if err := CheckEffectiveLaunchParameters(launch, nil); err != nil {
		t.Fatalf("an absent effective list was refused: %v", err)
	}
}

// TestEmbeddedCatalogMatchesOnTheLaunchArguments asserts a deployment that
// launches the way the measurement was launched matches the profile.
//
// IT IS THE PROOF THAT THE PROFILE IS REACHABLE, because a profile whose
// launch arguments no deployment can reproduce carries no capacity for
// anyone.
func TestEmbeddedCatalogMatchesOnTheLaunchArguments(t *testing.T) {
	catalog, err := LoadEmbedded()
	if err != nil {
		t.Fatalf("load embedded catalog: %v", err)
	}
	if len(catalog.Profiles) == 0 {
		t.Skip("the embedded catalog holds no profile")
	}

	profile := catalog.Profiles[0]
	request := Request{
		Role:          profile.Role,
		Identity:      profile.Identity,
		ServingConfig: profile.ServingConfig,
		Topology:      profile.Topology,
		Constraints: Constraints{
			TTFTTargetMilliseconds: 2000,
			ITLTargetMilliseconds:  100,
		},
	}

	if _, err := catalog.Resolve(request); err != nil {
		t.Fatalf("the profile did not resolve for a deployment that launches the measured way: %v", err)
	}
}

// TestEmbeddedCatalogRefusesTheEffectiveArgumentsAsLaunchArguments asserts a
// deployment that declares the engine's effective arguments does not match
// the profile.
//
// IT IS THE PROOF THAT THE EFFECTIVE ARGUMENTS ARE EVIDENCE AND NOT A LAUNCH
// SETTING, because the deployment asks for the launch arguments and the
// engine lowers one of them on its own.
func TestEmbeddedCatalogRefusesTheEffectiveArgumentsAsLaunchArguments(t *testing.T) {
	catalog, err := LoadEmbedded()
	if err != nil {
		t.Fatalf("load embedded catalog: %v", err)
	}
	if len(catalog.Profiles) == 0 {
		t.Skip("the embedded catalog holds no profile")
	}

	profile := catalog.Profiles[0]
	if len(profile.ServingConfig.EffectiveLaunchParameters) == 0 {
		t.Skip("the profile records no effective launch arguments")
	}
	if slices.Equal(profile.ServingConfig.LaunchParameters, profile.ServingConfig.EffectiveLaunchParameters) {
		t.Fatal("the profile launch arguments and its effective launch arguments are the same")
	}

	request := Request{
		Role:     profile.Role,
		Identity: profile.Identity,
		ServingConfig: ServingConfig{
			Router:              profile.ServingConfig.Router,
			KVCacheDType:        profile.ServingConfig.KVCacheDType,
			Offload:             profile.ServingConfig.Offload,
			PrefixCaching:       profile.ServingConfig.PrefixCaching,
			SpeculativeDecoding: profile.ServingConfig.SpeculativeDecoding,
			LaunchParameters:    slices.Clone(profile.ServingConfig.EffectiveLaunchParameters),
		},
		Topology: profile.Topology,
		Constraints: Constraints{
			TTFTTargetMilliseconds: 2000,
			ITLTargetMilliseconds:  100,
		},
	}

	if _, err := catalog.Resolve(request); err == nil {
		t.Fatal("expected the effective launch arguments to match no profile")
	}
}
