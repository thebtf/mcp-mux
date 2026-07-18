//go:build !windows && !linux

package procgroup

func prepareProcessTreeAuthority() error { return nil }

func drainWaitableGroupChildren(int) error { return nil }
