package cmd

import (
	"net/http"

	"ora/internal/ipctoken"
)

// authedDaemonGet fires a fire-and-forget GET at the local daemon with the IPC auth header attached — shared by the tray's pause/resume clicks (tray_linux.go, tray_windows.go), which otherwise duplicate this same "read token, attach header, GET" pattern per platform. Every daemon IPC handler except /ping requires the token now (see requireIPCToken in ipcauth.go); a bare http.Get here would just get 401'd.
func authedDaemonGet(url string) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return
	}
	if token, err := ipctoken.Read(ipctoken.DefaultPath); err == nil {
		req.Header.Set(ipctoken.HeaderName, token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}
