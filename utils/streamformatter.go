package utils

import (
	"encoding/json"
	"fmt"
	"sync"
)

type StreamFormatter struct {
	sync.RWMutex
	json bool
	used bool
}

func NewStreamFormatter(json bool) *StreamFormatter {
	return &StreamFormatter{
		json: json,
	}
}

func (sf *StreamFormatter) setUsed() {
	sf.Lock()
	defer sf.Unlock()

	sf.used = true
}

func (sf *StreamFormatter) Used() bool {
	sf.RLock()
	defer sf.RUnlock()

	return sf.used
}

func (sf *StreamFormatter) FormatStatus(id, format string, a ...interface{}) []byte {
	sf.setUsed()

	str := fmt.Sprintf(format, a...)
	if sf.json {
		b, err := json.Marshal(&JSONMessage{ID: id, Status: str})
		if err != nil {
			return sf.FormatError(err)
		}
		return b
	}
	return []byte(str + "\r\n")
}

func (sf *StreamFormatter) FormatError(err error) []byte {
	sf.setUsed()

	if sf.json {
		jsonError, ok := err.(*JSONError)
		if !ok {
			jsonError = &JSONError{Message: err.Error()}
		}
		if b, err := json.Marshal(&JSONMessage{Error: jsonError, ErrorMessage: err.Error()}); err == nil {
			return b
		}
		return []byte(`{"error":"format error"}`)
	}
	return []byte("Error: " + err.Error() + "\r\n")
}

func (sf *StreamFormatter) FormatProgress(id, action string, progress *JSONProgress) []byte {
	sf.setUsed()

	if progress == nil {
		progress = &JSONProgress{}
	}
	if sf.json {

		b, err := json.Marshal(&JSONMessage{
			Status:          action,
			ProgressMessage: progress.String(),
			Progress:        progress,
			ID:              id,
		})
		if err != nil {
			return nil
		}
		return b
	}
	endl := "\r"
	if progress.String() == "" {
		endl += "\n"
	}
	return []byte(action + " " + progress.String() + endl)
}
