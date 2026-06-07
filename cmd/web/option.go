package main

import (
	"errors"
	"net/http"
	"strings"
)

func (app *App) handleStraddleContracts(w http.ResponseWriter, r *http.Request) {
	start, end, err := requiredDateRange(r)
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}

	contracts, err := app.store.EligibleOptionContracts(start, end)
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"start_date": start.Format(dayLayout),
		"end_date":   end.Format(dayLayout),
		"contracts":  contracts,
		"count":      len(contracts),
	})
}

func (app *App) handleStraddleBacktest(w http.ResponseWriter, r *http.Request) {
	start, end, err := requiredDateRange(r)
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}

	callContract := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("call_contract")))
	putContract := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("put_contract")))
	if callContract == "" || putContract == "" {
		writeError(w, errors.New("call_contract and put_contract are required"), http.StatusBadRequest)
		return
	}
	callQty := intParam(r, "call_qty", 1)
	putQty := intParam(r, "put_qty", 1)
	if callQty <= 0 || putQty <= 0 {
		writeError(w, errors.New("call_qty and put_qty must be > 0"), http.StatusBadRequest)
		return
	}

	result, err := app.store.RunStraddleBacktest(start, end, callContract, putContract, callQty, putQty)
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, result)
}
