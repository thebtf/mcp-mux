//go:build !windows && !darwin

package procgroup

func processGroupProbePending(error) bool { return false }
