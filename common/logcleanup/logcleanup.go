package logcleanup

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pai801/myapi/common/logger"
	"github.com/pai801/myapi/model"
)

const DefaultRetentionHours = 168
const DefaultBodiesRetentionHours = 4

func RetentionHours() int {
	raw := strings.TrimSpace(os.Getenv("LOG_CLEAN_HOURS"))
	if raw == "" {
		return DefaultRetentionHours
	}

	hours, err := strconv.Atoi(raw)
	if err != nil {
		return DefaultRetentionHours
	}
	if hours == 0 {
		return 0
	}
	if hours < 0 {
		return DefaultRetentionHours
	}
	return hours
}

func BodiesRetentionHours() int {
	raw := strings.TrimSpace(os.Getenv("LOG_CLEAN_BODIES_HOURS"))
	if raw == "" {
		return DefaultBodiesRetentionHours
	}

	hours, err := strconv.Atoi(raw)
	if err != nil {
		return DefaultBodiesRetentionHours
	}
	if hours == 0 {
		return 0
	}
	if hours < 0 {
		return DefaultBodiesRetentionHours
	}
	return hours
}

func NextUTCHour(now time.Time) time.Time {
	utc := now.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day(), utc.Hour()+1, 0, 0, 0, time.UTC)
}

func CutoffTimestamp(now time.Time, hours int) int64 {
	utc := now.UTC()
	cutoff := time.Date(utc.Year(), utc.Month(), utc.Day(), utc.Hour(), 0, 0, 0, time.UTC).Add(-time.Duration(hours) * time.Hour)
	return cutoff.Unix()
}

func scheduleHourly(name string, hours int, task func(cutoff int64) (int64, error)) {
	if hours == 0 {
		logger.Log.Infof("%s cleanup disabled (retention hours = 0)", name)
		return
	}

	cleanup := func() {
		cutoff := CutoffTimestamp(time.Now(), hours)
		count, err := task(cutoff)
		if err != nil {
			logger.Log.Errorf("failed to clean old %s: %v", name, err)
			return
		}
		logger.Log.Infof("cleaned %d old %s with retention %d hours", count, name, hours)
	}

	cleanup()
	go func() {
		for {
			if sleepUntil := time.Until(NextUTCHour(time.Now())); sleepUntil > 0 {
				time.Sleep(sleepUntil)
			}
			cleanup()
		}
	}()
}

func Start() {
	scheduleHourly("logs", RetentionHours(), func(cutoff int64) (int64, error) {
		return model.DeleteOldLog(cutoff)
	})

	scheduleHourly("log bodies", BodiesRetentionHours(), func(cutoff int64) (int64, error) {
		return model.ClearOldLogBodies(cutoff)
	})
}
