package ipc

// Publish sends one event to every /events subscriber. It is how parts of the daemon outside this package — the component downloader, the updater — report progress on the stream the window already reads.
func (s *Server) Publish(ev Event) {
	if ev.Evidence == nil {
		ev.Evidence = []EvidenceItem{}
	}
	if ev.Actions == nil {
		ev.Actions = []ActionItem{}
	}
	s.hub.broadcast(ev)
}
