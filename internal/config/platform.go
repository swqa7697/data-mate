package config

import "runtime"

// ServiceFile is the owned native job descriptor, not a login registration.
func ServiceFile() string {
	if runtime.GOOS == "linux" {
		return "service.unit.json"
	}
	return "service.plist"
}

// RuntimeTemp is a fixed system directory independent of caller environment.
func RuntimeTemp() string {
	if runtime.GOOS == "linux" {
		return "/tmp"
	}
	return "/private/tmp"
}
