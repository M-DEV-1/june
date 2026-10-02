package embed

func (p *serverProcess) pid() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.aliveLocked() {
		return 0
	}
	return p.cmd.Process.Pid
}
