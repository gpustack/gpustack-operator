package args

// Args holds the options of the apireg-gen generator.
type Args struct {
	// OutputFile is the name of the generated file, including the ".go" suffix.
	OutputFile string

	// GoHeaderFile is the path to the boilerplate file that heads the generated file.
	GoHeaderFile string
}
