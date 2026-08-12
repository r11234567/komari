package terminal

import (
	"sync"

	"github.com/komari-monitor/komari/web/connection"
)

type TerminalSession struct {
	UUID        string
	UserUUID    string
	Browser     *connection.SafeConn
	Agent       *connection.SafeConn
	RequesterIp string
}

var TerminalSessionsMutex = &sync.Mutex{}
var TerminalSessions = make(map[string]*TerminalSession)

func CloseClientSessions(uuid string) {
	TerminalSessionsMutex.Lock()
	connections := make([]*connection.SafeConn, 0)
	for id, session := range TerminalSessions {
		if session.UUID != uuid {
			continue
		}
		delete(TerminalSessions, id)
		if session.Browser != nil {
			connections = append(connections, session.Browser)
		}
		if session.Agent != nil {
			connections = append(connections, session.Agent)
		}
	}
	TerminalSessionsMutex.Unlock()
	for _, connection := range connections {
		_ = connection.Close()
	}
}
