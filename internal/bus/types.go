// Package bus defines the message types that flow between agent connections
// and the single terminal-owning goroutine in main. See chorus-spec.md §7.
package bus

import acp "github.com/coder/acp-go-sdk"

// Update is a session/update notification tagged with which agent sent it.
type Update struct {
	Agent        string
	Notification acp.SessionNotification
}

// PermissionRequest is a session/request_permission call tagged with which
// agent asked. Resp carries the chosen outcome back to the blocked
// RequestPermission call that created it.
type PermissionRequest struct {
	Agent string
	Req   acp.RequestPermissionRequest
	Resp  chan acp.RequestPermissionResponse
}
