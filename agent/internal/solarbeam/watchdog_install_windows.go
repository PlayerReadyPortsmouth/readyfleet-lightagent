package solarbeam

// Processes that mean a driver is being installed or removed. Heuristic: a
// false "busy" only delays a restart; a false "idle" could restart through a
// display reset. Confirm the NVIDIA installer's process names on PC-1 canary.
var driverInstallProcs = []string{"drvinst.exe", "pnputil.exe", "nvsetup.exe", "nvcontainer-installer.exe"}

func installInProgress() bool {
	for _, p := range driverInstallProcs {
		if processExistsByName(p) {
			return true
		}
	}
	return false
}
