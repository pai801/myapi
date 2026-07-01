package common

import (
	"fmt"
)

func LogQuota(quota int64) string {
	return fmt.Sprintf("%d 点额度", quota)
}
