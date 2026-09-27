package commerce

import (
	"errors"
	"time"
)

// Keep the existing ten-year monthly range, with year as the largest unit.
// Months remains a compatibility field; new callers use the explicit pair.
func normalizePlanDuration(p *Plan) error {
	if p.Kind == "addon" {
		if p.Months != 0 || p.DurationUnit != "" || p.DurationValue != 0 {
			return errors.New("add-ons cannot set a duration")
		}
		return nil
	}
	if p.Kind != "period" {
		return errors.New("invalid plan kind")
	}
	if p.DurationUnit == "" && p.DurationValue == 0 {
		p.DurationUnit, p.DurationValue = "month", p.Months
	}
	var max, months int
	switch p.DurationUnit {
	case "day":
		max = 3650
	case "week":
		max = 520
	case "month":
		max = 120
	case "year":
		max = 10
	default:
		return errors.New("invalid plan duration unit")
	}
	if p.DurationValue < 1 || p.DurationValue > max {
		return errors.New("invalid plan duration value")
	}
	if p.DurationUnit == "month" {
		months = p.DurationValue
	} else if p.DurationUnit == "year" {
		months = p.DurationValue * 12
	}
	if p.Months != 0 && p.Months != months {
		return errors.New("conflicting plan duration")
	}
	p.Months = months
	return nil
}

// All entitlement creation paths share the same calendar arithmetic.
func planExpiry(start time.Time, p Plan) (time.Time, error) {
	if err := normalizePlanDuration(&p); err != nil {
		return time.Time{}, err
	}
	if p.Kind != "period" {
		return time.Time{}, errors.New("period plan required")
	}
	if p.DurationUnit == "month" || p.DurationUnit == "year" {
		return AddMonths(start, p.Months), nil
	}
	days := p.DurationValue
	if p.DurationUnit == "week" {
		days *= 7
	}
	loc, _ := time.LoadLocation("Asia/Shanghai")
	return start.In(loc).AddDate(0, 0, days).UTC(), nil
}
