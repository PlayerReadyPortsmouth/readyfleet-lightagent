package solarbeam

import "time"

func stopBridgeForLinkRotationWith(name string, terminate func(string), exists func(string) (bool, error)) error {
	if name == "" {
		return errPreviousBridgeStillRunning
	}
	terminate(name)
	deadline := time.Now().Add(5 * time.Second)
	for {
		found, err := exists(name)
		if err != nil {
			return errPreviousBridgeStillRunning
		}
		if !found {
			return nil
		}
		if time.Now().After(deadline) {
			return errPreviousBridgeStillRunning
		}
		time.Sleep(50 * time.Millisecond)
	}
}
