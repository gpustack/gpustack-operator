package sim

// Fault is a bounded synthetic scrape outage for one ordinal, or all ordinals when negative.
type Fault struct {
	FromSeconds  float64
	UntilSeconds float64
	Ordinal      int
}

// Scenario fixes traffic, plant and lifecycle inputs independently of controller parameters.
type Scenario struct {
	Name             string
	Holdout          bool
	DurationSeconds  float64
	StepSeconds      float64
	Engine           string
	Traffic          TrafficConfig
	Plant            PlantConfig
	Fleet            FleetConfig
	Faults           []Fault
	ResetAtSeconds   float64
	RestartAtSeconds float64
	AbortRetirement  bool
}

// Fixture freezes experiment partitions and comparison criteria before candidate tuning.
type Fixture struct {
	Synthetic       bool
	Version         int
	Tolerances      Tolerances
	CapacityFactors []float64
	Scenarios       []Scenario
}

// Tolerances define acceptable paired tradeoffs, not a physical performance guarantee.
type Tolerances struct {
	MaximumCostIncreaseFraction float64
	MaximumSLODecreaseFraction  float64
	MaximumSteadyChanges        int
}
