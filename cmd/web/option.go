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

func (app *App) handleContinuousStraddleBacktest(w http.ResponseWriter, r *http.Request) {
	start, end, err := requiredDateRange(r)
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}

	holdDays := intParam(r, "hold_days", 10)
	minDTE := intParam(r, "min_dte", 30)
	restDays := intParam(r, "rest_days", 1)
	atrFilterMode := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("atr_filter_mode")))
	if atrFilterMode == "" {
		atrFilterMode = "fixed"
	}
	sellProfit := floatParam(r, "sell_profit", 0.2)
	maxATRPercent := floatParam(r, "max_atr_pct", 2.0)
	backoffDays := intParam(r, "backoff_days", 14)
	if sellProfit > 1 {
		sellProfit = sellProfit / 100
	}
	if holdDays <= 0 {
		writeError(w, errors.New("hold_days must be > 0"), http.StatusBadRequest)
		return
	}
	if minDTE < 0 {
		writeError(w, errors.New("min_dte must be >= 0"), http.StatusBadRequest)
		return
	}
	if restDays < 0 {
		writeError(w, errors.New("rest_days must be >= 0"), http.StatusBadRequest)
		return
	}
	if sellProfit <= 0 {
		writeError(w, errors.New("sell_profit must be > 0"), http.StatusBadRequest)
		return
	}
	if atrFilterMode != "fixed" && atrFilterMode != "median" {
		writeError(w, errors.New("atr_filter_mode must be fixed or median"), http.StatusBadRequest)
		return
	}
	if atrFilterMode == "fixed" && maxATRPercent <= 0 {
		writeError(w, errors.New("max_atr_pct must be > 0 when atr_filter_mode=fixed"), http.StatusBadRequest)
		return
	}
	if atrFilterMode == "median" && backoffDays <= 0 {
		writeError(w, errors.New("backoff_days must be > 0 when atr_filter_mode=median"), http.StatusBadRequest)
		return
	}

	result, err := app.store.RunContinuousStraddleBacktest(start, end, holdDays, minDTE, sellProfit, restDays, atrFilterMode, maxATRPercent, backoffDays)
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, result)
}
