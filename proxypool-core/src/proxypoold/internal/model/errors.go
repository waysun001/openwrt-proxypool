package model

type CodeError struct {
	Code    string
	Message string
}

func (e *CodeError) Error() string {
	return e.Code + ": " + e.Message
}
