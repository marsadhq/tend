package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// EnvDeliveryHoldMinutes sets how long a failing notification may hold back
// the later ones on its channel (ordered delivery), in whole minutes counted
// from its first attempt. 0 turns per-channel ordering off.
const EnvDeliveryHoldMinutes = "TEND_DELIVERY_HOLD_MINUTES"

// DefaultDeliveryHoldMinutes is the hold when the variable is unset. It
// matches kit/notify.DefaultHold.
const DefaultDeliveryHoldMinutes = 15

// maxDeliveryHoldMinutes is a day: a delivery is given up after that anyway.
const maxDeliveryHoldMinutes = 24 * 60

// LoadDeliveryHold reads TEND_DELIVERY_HOLD_MINUTES, defaulting to 15
// minutes. A value that is not a whole number of minutes from 0 to 1440 is an
// error naming the variable.
func LoadDeliveryHold() (time.Duration, error) {
	minutes := DefaultDeliveryHoldMinutes
	if v := strings.TrimSpace(os.Getenv(EnvDeliveryHoldMinutes)); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > maxDeliveryHoldMinutes {
			return 0, fmt.Errorf("%s=%q: want a whole number of minutes from 0 (no per-channel ordering) to %d", EnvDeliveryHoldMinutes, v, maxDeliveryHoldMinutes)
		}
		minutes = n
	}
	return time.Duration(minutes) * time.Minute, nil
}
