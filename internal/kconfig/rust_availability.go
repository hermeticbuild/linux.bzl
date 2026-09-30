package kconfig

import "strings"

func (e *LinuxProbeEvaluator) pythonLXMLProbe(command string) (linuxProbeTruth, bool, error) {
	const arguments = `-c "import lxml"`
	if command == arguments {
		return knownLinuxProbeTruth(false), true, nil
	}
	tool, found := strings.CutSuffix(command, " "+arguments)
	if !found {
		return linuxProbeTruth{}, false, nil
	}
	tool = strings.TrimSpace(tool)
	if e.tools["python3"] == "" || !e.isToolToken(tool, "python3") {
		return linuxProbeTruth{}, true, e.unsupportedCommand(command)
	}
	request := ProbeRequest{
		Schema:  LinuxProbeRequestSchema,
		Steps:   []ProbeStep{{Name: "import-lxml", Tool: "python3", Arguments: []string{"-c", "import lxml"}}},
		Outcome: ProbeOutcome{Kind: "boolean", Predicate: &ProbePredicate{Operator: "exit-zero", Step: "import-lxml"}},
	}
	truth, err := e.requestTruth(request)
	return truth, true, err
}
