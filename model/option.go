package model

import (
	"encoding/json"
	"sync"

	"strconv"
	"strings"
	"time"

	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/logger"
	"github.com/songquanpeng/one-api/relay/apitype"
	billingratio "github.com/songquanpeng/one-api/relay/billing/ratio"
)

const ModelEndpointTypesKey = "ModelEndpointTypes"

var modelEndpointTypesMap = make(map[string][]apitype.EndpointType)
var modelEndpointTypesLock sync.RWMutex

type Option struct {
	Key   string `json:"key" gorm:"primaryKey"`
	Value string `json:"value"`
}

func AllOption() ([]*Option, error) {
	var options []*Option
	var err error
	err = DB.Find(&options).Error
	return options, err
}

func InitOptionMap() {
	config.OptionMapRWMutex.Lock()
	config.OptionMap = make(map[string]string)
	config.OptionMap["PasswordLoginEnabled"] = strconv.FormatBool(config.PasswordLoginEnabled)
	config.OptionMap["TurnstileCheckEnabled"] = strconv.FormatBool(config.TurnstileCheckEnabled)
	config.OptionMap["AutomaticDisableChannelEnabled"] = strconv.FormatBool(config.AutomaticDisableChannelEnabled)
	config.OptionMap["AutomaticEnableChannelEnabled"] = strconv.FormatBool(config.AutomaticEnableChannelEnabled)
	config.OptionMap["ApproximateTokenEnabled"] = strconv.FormatBool(config.ApproximateTokenEnabled)
	config.OptionMap["LogConsumeEnabled"] = strconv.FormatBool(config.LogConsumeEnabled)
	config.OptionMap["DisplayTokenStatEnabled"] = strconv.FormatBool(config.DisplayTokenStatEnabled)
	config.OptionMap["ChannelDisableThreshold"] = strconv.FormatFloat(config.ChannelDisableThreshold, 'f', -1, 64)
	config.OptionMap["About"] = ""
	config.OptionMap["HomePageContent"] = ""
	config.OptionMap["Footer"] = config.Footer
	config.OptionMap["SystemName"] = config.SystemName
	config.OptionMap["Logo"] = config.Logo
	config.OptionMap["ServerAddress"] = ""
	config.OptionMap["MessagePusherAddress"] = ""
	config.OptionMap["MessagePusherToken"] = ""
	config.OptionMap["TurnstileSiteKey"] = ""
	config.OptionMap["TurnstileSecretKey"] = ""
	config.OptionMap["QuotaRemindThreshold"] = strconv.FormatInt(config.QuotaRemindThreshold, 10)
	config.OptionMap["ModelRatio"] = billingratio.ModelRatio2JSONString()
	config.OptionMap["GroupRatio"] = billingratio.GroupRatio2JSONString()
	config.OptionMap["CompletionRatio"] = billingratio.CompletionRatio2JSONString()
	config.OptionMap["QuotaPerUnit"] = strconv.FormatFloat(config.QuotaPerUnit, 'f', -1, 64)
	config.OptionMap["RetryTimes"] = strconv.Itoa(config.RetryTimes)
	config.OptionMapRWMutex.Unlock()
	loadOptionsFromDatabase()
}

func loadOptionsFromDatabase() {
	options, _ := AllOption()
	for _, option := range options {
		if option.Key == "ModelRatio" {
			option.Value = billingratio.AddNewMissingRatio(option.Value)
		}
		err := updateOptionMap(option.Key, option.Value)
		if err != nil {
			logger.Log.Errorf("failed to update option map: " + err.Error())
		}
	}
}

func SyncOptions(frequency int) {
	for {
		time.Sleep(time.Duration(frequency) * time.Second)
		logger.Log.Debugf("syncing options from database")
		loadOptionsFromDatabase()
	}
}

func UpdateOption(key string, value string) error {
	// Save to database first
	option := Option{
		Key: key,
	}
	// https://gorm.io/docs/update.html#Save-All-Fields
	DB.FirstOrCreate(&option, Option{Key: key})
	option.Value = value
	// Save is a combination function.
	// If save value does not contain primary key, it will execute Create,
	// otherwise it will execute Update (with all fields).
	DB.Save(&option)
	// Update OptionMap
	return updateOptionMap(key, value)
}

func updateOptionMap(key string, value string) (err error) {
	config.OptionMapRWMutex.Lock()
	defer config.OptionMapRWMutex.Unlock()
	config.OptionMap[key] = value
	if strings.HasSuffix(key, "Enabled") {
		boolValue := value == "true"
		switch key {
		case "PasswordLoginEnabled":
			config.PasswordLoginEnabled = boolValue
		case "TurnstileCheckEnabled":
			config.TurnstileCheckEnabled = boolValue
		case "AutomaticDisableChannelEnabled":
			config.AutomaticDisableChannelEnabled = boolValue
		case "AutomaticEnableChannelEnabled":
			config.AutomaticEnableChannelEnabled = boolValue
		case "ApproximateTokenEnabled":
			config.ApproximateTokenEnabled = boolValue
		case "LogConsumeEnabled":
			config.LogConsumeEnabled = boolValue
		case "DisplayTokenStatEnabled":
			config.DisplayTokenStatEnabled = boolValue
		}
	}
	switch key {
	case "ServerAddress":
		config.ServerAddress = value
	case "Footer":
		config.Footer = value
	case "SystemName":
		config.SystemName = value
	case "Logo":
		config.Logo = value
	case "MessagePusherAddress":
		config.MessagePusherAddress = value
	case "MessagePusherToken":
		config.MessagePusherToken = value
	case "TurnstileSiteKey":
		config.TurnstileSiteKey = value
	case "TurnstileSecretKey":
		config.TurnstileSecretKey = value
	case "QuotaRemindThreshold":
		config.QuotaRemindThreshold, _ = strconv.ParseInt(value, 10, 64)
	case "RetryTimes":
		config.RetryTimes, _ = strconv.Atoi(value)
	case "ModelRatio":
		err = billingratio.UpdateModelRatioByJSONString(value)
	case "GroupRatio":
		err = billingratio.UpdateGroupRatioByJSONString(value)
	case "CompletionRatio":
		err = billingratio.UpdateCompletionRatioByJSONString(value)
	case "ChannelDisableThreshold":
		config.ChannelDisableThreshold, _ = strconv.ParseFloat(value, 64)
	case "QuotaPerUnit":
		config.QuotaPerUnit, _ = strconv.ParseFloat(value, 64)
	case ModelEndpointTypesKey:
		updateModelEndpointTypesMap(value)
	}
	return err
}

func updateModelEndpointTypesMap(jsonValue string) {
	modelEndpointTypesLock.Lock()
	defer modelEndpointTypesLock.Unlock()

	modelEndpointTypesMap = make(map[string][]apitype.EndpointType)
	if jsonValue == "" {
		return
	}

	var rawMap map[string][]string
	if err := json.Unmarshal([]byte(jsonValue), &rawMap); err != nil {
		logger.Log.Errorf("failed to parse ModelEndpointTypes: " + err.Error())
		return
	}

	for simplifiedName, endpoints := range rawMap {
		endpointTypes := make([]apitype.EndpointType, 0, len(endpoints))
		for _, ep := range endpoints {
			endpointTypes = append(endpointTypes, apitype.EndpointType(ep))
		}
		modelEndpointTypesMap[simplifiedName] = endpointTypes
	}
}

func GetModelEndpointTypes(modelName string) []apitype.EndpointType {
	simplifiedName := SimplifyModelName(modelName)
	modelEndpointTypesLock.RLock()
	defer modelEndpointTypesLock.RUnlock()
	if endpoints, ok := modelEndpointTypesMap[simplifiedName]; ok {
		return endpoints
	}
	return []apitype.EndpointType{apitype.EndpointTypeOpenAI}
}
