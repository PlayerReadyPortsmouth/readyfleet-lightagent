package svc

// ServiceName is the Windows service identifier under which the agent
// registers with the SCM. Keep it stable across releases — it's used
// by `agent.exe install`, `agent.exe uninstall`, and the MSI's
// ServiceInstall element.
const ServiceName = "ReadyFleetAgent"

// ServiceDisplayName is shown in the Services control panel.
const ServiceDisplayName = "ReadyFleet Agent"

// ServiceDescription is shown in the Services control panel's Description
// column.
const ServiceDescription = "Player Ready ReadyFleet remote management agent."
