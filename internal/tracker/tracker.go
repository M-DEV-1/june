package tracker

// defines standard interface for all os implementations
type Tracker interface {
	GetActiveWindow() (app string, title string, err error)
}

// New is our generic constructor, defined in os-specific files
