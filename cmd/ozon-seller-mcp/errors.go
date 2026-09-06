package main

import (
	"errors"

	"github.com/MainAlexStark/ozon-seller-mcp/ozon"
)

// errorsAs — локальная обёртка, чтобы main.go читался без шума.
func errorsAs(err error, target **ozon.APIError) bool {
	return errors.As(err, target)
}
