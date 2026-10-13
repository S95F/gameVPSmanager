//go:build !linux

package listeners

func byName([]string) ([]Entry, error) { return nil, nil }
