//go:build windows

package sacposture

import (
	"errors"
	"strconv"

	"golang.org/x/sys/windows/registry"
)

type registryReader struct{}

func systemReader() Reader { return registryReader{} }

func (registryReader) PolicyState() (uint64, bool, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SYSTEM\CurrentControlSet\Control\CI\Policy`, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if errors.Is(err, registry.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue("VerifiedAndReputablePolicyState")
	if errors.Is(err, registry.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return v, true, nil
}

func (registryReader) OSBuild() int {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return 0
	}
	defer k.Close()
	s, _, err := k.GetStringValue("CurrentBuild")
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(s)
	return n
}
