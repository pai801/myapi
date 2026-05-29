package main

import (
	"fmt"
	"log"
	"strings"

	_ "github.com/joho/godotenv/autoload"

	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/logger"
	"github.com/songquanpeng/one-api/model"
)

func main() {
	common.Init()
	logger.SetupLogger()

	model.InitDB()
	defer func() {
		err := model.CloseDB()
		if err != nil {
			log.Fatalf("failed to close database: %v", err)
		}
	}()

	channels, err := model.GetAllChannels(0, -1, "all")
	if err != nil {
		log.Fatalf("failed to query channels: %v", err)
	}

	var updated int
	for _, ch := range channels {
		if ch.Models == "" || ch.ModelsAlias != "" {
			continue
		}

		modelNames := ch.GetModels()
		simplified := make([]string, 0, len(modelNames))
		for _, name := range modelNames {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			simplified = append(simplified, model.SimplifyModelName(name))
		}

		if len(simplified) == 0 {
			continue
		}

		alias := strings.Join(simplified, ",")
		if err := model.DB.Model(ch).Update("models_alias", alias).Error; err != nil {
			log.Printf("failed to update channel %d: %v", ch.Id, err)
			continue
		}
		updated++
		fmt.Printf("channel %d: models=%q -> alias=%q\n", ch.Id, ch.Models, alias)
	}

	fmt.Printf("\ndone! updated %d channels\n", updated)
}
