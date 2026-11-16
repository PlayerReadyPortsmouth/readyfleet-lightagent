//go:build !windows

package winpaths

import "os"

// Off Windows these only matter to tests and dev builds.

func ProgramFiles() string             { return os.Getenv("ProgramFiles") }
func ProgramData() string              { return os.Getenv("ProgramData") }
func Windows() string                  { return os.Getenv("SystemRoot") }
func UserProfiles() string             { return "" }
func SystemDrive() string              { return "" }
func solarBeamInstallLocation() string { return "" }
