package main

import (
	"errors"
	"net/http"
	"time"
)

func requiredDateRange(r *http.Request) (time.Time, time.Time, error) {
	start, err := requiredDate(r.URL.Query().Get("start"), "start")
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	end, err := requiredDate(r.URL.Query().Get("end"), "end")
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if end.Before(start) {
		return time.Time{}, time.Time{}, errors.New("end must be >= start")
	}
	return start, end, nil
}
