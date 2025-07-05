package common

import (
	"encoding/json"
	"log/slog"
	"time"
)

type SleeperRequest struct {
	GatewaySleepFor Duration `json:"gatewaySleepFor"`
	SleeperSleepFor Duration `json:"sleeperSleepFor"`
}

type Duration time.Duration

func (d *Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(*d).String())
}

func (d *Duration) UnmarshalJSON(data []byte) error {
	var str string
	if err := json.Unmarshal(data, &str); err != nil {
		return err
	}
	slog.Info(str, string(data))
	val, err := time.ParseDuration(str)
	*d = Duration(val)
	return err
}
