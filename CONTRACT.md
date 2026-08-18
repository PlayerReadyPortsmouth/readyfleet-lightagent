# API contract

Every command this agent will accept from the ReadyFleet controller.
Generated from the source in this repository — if a command is not
listed here, there is no code in this binary that can act on it.

| Command | Wire value |
|---|---|
| `CmdInventoryRefresh` | "inventory_refresh" |
| `CmdShowNotification` | "show_notification" |
| `CmdSolarbeamEngineUpdate` | "solarbeam_engine_update" |
| `CmdStartSolarbeam` | "start_solarbeam" |
| `CmdStopSolarbeam` | "stop_solarbeam" |
| `CmdUpdate` | "update" |

## Transport

Outbound only: the agent dials the controller over TLS and authenticates
with a per-machine client certificate issued at enrolment. It listens on
no port and accepts no inbound connection.
