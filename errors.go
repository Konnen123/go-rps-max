package main

import "errors"

var (
	ErrInvalidLocationData = errors.New("invalid location data: city and country are required")
)
