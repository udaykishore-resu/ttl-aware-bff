package api

import "github.com/udaykishore/ttl-aware-bff/pkg/errs"

// notFoundErr is the catch-all for an unmatched route. It uses the same error
// document as every other failure, so a client never has to parse Go's default
// plain-text 404.
func notFoundErr() error {
	return errs.New(errs.CodeNotFound, "no such endpoint").WithOp("api.route")
}
