package common

import "net/http"

type statusCoder interface {
	statusCode() int
}

type AppError struct {
	error
	code int
}

func (e AppError) statusCode() int {
	return e.code
}

func NewAppError(err error, code int) AppError {
	return AppError{
		error: err,
		code:  code,
	}
}

func HandleError(rw http.ResponseWriter, err error) {
	if statusErr, ok := err.(statusCoder); ok {
		http.Error(rw, err.Error(), statusErr.statusCode())
		return
	}
	http.Error(rw, err.Error(), http.StatusInternalServerError)
	return
}
