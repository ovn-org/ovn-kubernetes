package util

// GetChildStopChan returns a new channel that doesn't affect parentStopChan, but will be closed when
// parentStopChan is closed. May be used for child goroutines that may need to be stopped with the main goroutine or
// separately.
func GetChildStopChan(parentStopChan <-chan struct{}) chan struct{} {
	childStopChan := make(chan struct{})

	select {
	case <-parentStopChan:
		// parent is already canceled
		close(childStopChan)
		return childStopChan
	default:
	}

	go func() {
		select {
		case <-parentStopChan:
			close(childStopChan)
			return
		case <-childStopChan:
			return
		}
	}()
	return childStopChan
}
