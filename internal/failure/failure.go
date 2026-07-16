package failure

import "time"

type Error struct {
	Category   string
	Retryable  bool
	Obsolete   bool
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	return e.Category
}

func Failed(category string) error {
	return &Error{Category: category}
}

func Retry(category string, retryAfter time.Duration) error {
	return &Error{Category: category, Retryable: true, RetryAfter: retryAfter}
}

func Obsolete(category string) error {
	return &Error{Category: category, Obsolete: true}
}
