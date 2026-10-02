package proactive

// waitRoutines blocks until every routine run this scheduler has started has finished. Only tests call it: production starts a run and lets the tick return.
func (s *Scheduler) waitRoutines() {
	s.running.Wait()
}
