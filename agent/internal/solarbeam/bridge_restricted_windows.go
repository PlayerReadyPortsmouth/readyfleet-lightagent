//go:build windows

package solarbeam

// Restricted bridge launch (Paths.RestrictedBridge, config.json
// `solarbeam_restricted_bridge`, ModeManaged only, default off).
//
// The bridge parses untrusted network input (STUN/DTLS/SRTP/RTCP/SCTP via
// pion) and needs no privilege: it dials ReadyApp and LiveKit, listens on
// loopback 9092 and LAN UDP, and writes logs only through the stdout pipe the
// agent drains. Launched by the SYSTEM agent it used to be SYSTEM too. With the
// setting on it runs as:
//
//   - NT AUTHORITY\LocalService from a fresh service logon, so the token
//     carries a logon SID unique to this launch;
//   - with every privilege removed except SeChangeNotify (DISABLE_MAX_PRIVILEGE
//     drops SeImpersonate, which would otherwise be a known path back to SYSTEM);
//   - at Low integrity, so it cannot write anywhere LocalService or Users could
//     (no planting files a SYSTEM service later loads) or open other processes
//     for write;
//   - with process, thread and default-object DACLs granting only SYSTEM and
//     that per-launch logon SID, so other LocalService processes cannot read
//     its memory or environment;
//   - in a job that allows one active process, so a compromised bridge cannot
//     start anything;
//   - on its own logon-session window station ("" desktop), never the agent's.
//
// The engine stays SYSTEM (secure-desktop capture and injection) and keeps
// reading the SYSTEM/Administrators-only link-auth file. That file is NOT
// widened for the bridge: the bridge gets the same launch key in its direct
// environment instead (see provisionPlatformLinkAuth), which the process DACL
// above limits to SYSTEM and the bridge itself, and which the bridge consumes
// and clears before doing anything else.

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modAdvapi32               = windows.NewLazySystemDLL("advapi32.dll")
	procLogonUserW            = modAdvapi32.NewProc("LogonUserW")
	procCreateRestrictedToken = modAdvapi32.NewProc("CreateRestrictedToken")
)

const (
	logon32LogonService    = 5
	logon32ProviderDefault = 0
	disableMaxPrivilege    = 0x1
)

// startRestrictedBridgeFn is the seam launchBridge calls; tests replace it so
// they never create a logon session or a process.
var startRestrictedBridgeFn = startRestrictedBridge

// restrictedBridgeSDDL grants SYSTEM (the agent: terminate, wait, query) and
// the bridge's own logon SID, nothing else.
func restrictedBridgeSDDL(logonSID *windows.SID) string {
	return "D:P(A;;GA;;;SY)(A;;GA;;;" + logonSID.String() + ")"
}

// restrictedBridgeToken returns a primary token for the bridge and its logon
// SID. Requires SeTcbPrivilege (the agent runs as LocalSystem).
func restrictedBridgeToken() (windows.Token, *windows.SID, error) {
	user, _ := windows.UTF16PtrFromString("LocalService")
	domain, _ := windows.UTF16PtrFromString("NT AUTHORITY")
	var logon windows.Token
	if r, _, err := procLogonUserW.Call(uintptr(unsafe.Pointer(user)), uintptr(unsafe.Pointer(domain)), 0,
		logon32LogonService, logon32ProviderDefault, uintptr(unsafe.Pointer(&logon))); r == 0 {
		return 0, nil, fmt.Errorf("log on LocalService: %w", err)
	}
	defer logon.Close()
	var token windows.Token
	if r, _, err := procCreateRestrictedToken.Call(uintptr(logon), disableMaxPrivilege,
		0, 0, 0, 0, 0, 0, uintptr(unsafe.Pointer(&token))); r == 0 {
		return 0, nil, fmt.Errorf("restrict token: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			token.Close()
		}
	}()
	groups, err := token.GetTokenGroups()
	if err != nil {
		return 0, nil, fmt.Errorf("token groups: %w", err)
	}
	var logonSID *windows.SID
	for _, g := range groups.AllGroups() {
		if g.Attributes&windows.SE_GROUP_LOGON_ID == windows.SE_GROUP_LOGON_ID {
			if logonSID, err = g.Sid.Copy(); err != nil {
				return 0, nil, err
			}
			break
		}
	}
	if logonSID == nil {
		return 0, nil, fmt.Errorf("restricted bridge token has no logon SID")
	}
	low, err := windows.CreateWellKnownSid(windows.WinLowLabelSid)
	if err != nil {
		return 0, nil, err
	}
	label := windows.Tokenmandatorylabel{Label: windows.SIDAndAttributes{Sid: low, Attributes: windows.SE_GROUP_INTEGRITY}}
	if err := windows.SetTokenInformation(token, windows.TokenIntegrityLevel,
		(*byte)(unsafe.Pointer(&label)), label.Size()); err != nil {
		return 0, nil, fmt.Errorf("set low integrity: %w", err)
	}
	sd, err := windows.SecurityDescriptorFromString(restrictedBridgeSDDL(logonSID))
	if err != nil {
		return 0, nil, err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return 0, nil, err
	}
	defaultDACL := struct{ DefaultDacl *windows.ACL }{dacl} // TOKEN_DEFAULT_DACL
	if err := windows.SetTokenInformation(token, windows.TokenDefaultDacl,
		(*byte)(unsafe.Pointer(&defaultDACL)), uint32(unsafe.Sizeof(defaultDACL))); err != nil {
		return 0, nil, fmt.Errorf("set default DACL: %w", err)
	}
	runtime.KeepAlive(sd)
	ok = true
	return token, logonSID, nil
}

// startRestrictedBridge launches path with env under the restricted token,
// copying its stdout/stderr to out. copied closes once the output pipe is
// drained (after the process exits).
func startRestrictedBridge(path string, env []string, out io.Writer) (proc *os.Process, copied <-chan struct{}, err error) {
	token, logonSID, err := restrictedBridgeToken()
	if err != nil {
		return nil, nil, err
	}
	defer token.Close()
	sd, err := windows.SecurityDescriptorFromString(restrictedBridgeSDDL(logonSID))
	if err != nil {
		return nil, nil, err
	}
	sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}

	// Only the pipe's write end is inheritable, and the handle list below
	// limits inheritance to exactly that handle.
	var r, w windows.Handle
	inherit := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), InheritHandle: 1}
	if err := windows.CreatePipe(&r, &w, &inherit, 0); err != nil {
		return nil, nil, fmt.Errorf("bridge output pipe: %w", err)
	}
	defer windows.CloseHandle(w)
	reader := os.NewFile(uintptr(r), "bridge output")
	if err := windows.SetHandleInformation(r, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		reader.Close()
		return nil, nil, err
	}
	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		reader.Close()
		return nil, nil, err
	}
	defer attrs.Delete()
	if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&w), unsafe.Sizeof(w)); err != nil {
		reader.Close()
		return nil, nil, err
	}

	desktop := []uint16{0} // "": the bridge's own logon-session window station
	si := windows.StartupInfoEx{
		StartupInfo: windows.StartupInfo{
			Cb:        uint32(unsafe.Sizeof(windows.StartupInfoEx{})),
			Desktop:   &desktop[0],
			Flags:     windows.STARTF_USESTDHANDLES,
			StdOutput: w,
			StdErr:    w,
		},
		ProcThreadAttributeList: attrs.List(),
	}
	block, err := windowsEnvBlock(env)
	defer clear(block) // holds the link-auth key
	if err != nil {
		reader.Close()
		return nil, nil, err
	}
	app, err := windows.UTF16PtrFromString(path)
	if err != nil {
		reader.Close()
		return nil, nil, err
	}
	cmdline, err := windows.UTF16PtrFromString(windows.EscapeArg(path))
	if err != nil {
		reader.Close()
		return nil, nil, err
	}
	var pi windows.ProcessInformation
	// DETACHED_PROCESS: no console, so no conhost.exe for the one-process job.
	flags := uint32(windows.CREATE_SUSPENDED | windows.DETACHED_PROCESS |
		windows.CREATE_UNICODE_ENVIRONMENT | windows.EXTENDED_STARTUPINFO_PRESENT)
	if err := windows.CreateProcessAsUser(token, app, cmdline, &sa, &sa, true, flags,
		&block[0], nil, &si.StartupInfo, &pi); err != nil {
		reader.Close()
		return nil, nil, fmt.Errorf("create restricted bridge: %w", err)
	}
	defer windows.CloseHandle(pi.Thread)
	defer windows.CloseHandle(pi.Process)
	fail := func(err error) (*os.Process, <-chan struct{}, error) {
		_ = windows.TerminateProcess(pi.Process, 1)
		reader.Close()
		return nil, nil, err
	}
	if err := confineBridgeProcess(pi.Process); err != nil {
		return fail(fmt.Errorf("confine restricted bridge: %w", err))
	}
	// Open our handle while the process is still suspended, so the pid cannot
	// have been reused.
	if proc, err = os.FindProcess(int(pi.ProcessId)); err != nil {
		return fail(err)
	}
	if _, err := windows.ResumeThread(pi.Thread); err != nil {
		proc.Release()
		return fail(fmt.Errorf("resume restricted bridge: %w", err))
	}
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(out, reader)
		reader.Close()
		close(done)
	}()
	return proc, done, nil
}

// confineBridgeProcess puts the bridge in a job allowing exactly one active
// process. No kill-on-close: the bridge must outlive an agent restart
// (sessionowner.go adopts it).
func confineBridgeProcess(process windows.Handle) error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(job)
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
		LimitFlags:         windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS | windows.JOB_OBJECT_LIMIT_DIE_ON_UNHANDLED_EXCEPTION,
		ActiveProcessLimit: 1,
	}}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return err
	}
	return windows.AssignProcessToJobObject(job, process)
}

// windowsEnvBlock builds a CREATE_UNICODE_ENVIRONMENT block.
func windowsEnvBlock(env []string) ([]uint16, error) {
	var block []uint16
	for _, kv := range env {
		u, err := windows.UTF16FromString(kv)
		if err != nil {
			return block, err
		}
		block = append(block, u...)
	}
	if len(block) == 0 {
		block = append(block, 0)
	}
	return append(block, 0), nil
}
