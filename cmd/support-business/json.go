package main

import (
	"github.com/xjfyrh/jobforge/internal/jsonstrict"
)

func decodeJSON(data []byte, target any) error {
	return jsonstrict.Decode(data, target)
}
