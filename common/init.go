package common

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/pai801/myapi/common/config"
	"github.com/pai801/myapi/common/logger"
)

var (
	Port         = flag.Int("port", 3000, "the listening port")
	PrintVersion = flag.Bool("version", false, "print version and exit")
	PrintHelp    = flag.Bool("help", false, "print help and exit")
	LogDir       = flag.String("log-dir", "", "specify the log directory")
)

func printHelp() {
	fmt.Println("MyAPI " + Version + " - All in one API service for OpenAI API.")
	fmt.Println("Copyright (C) 2026 Pai801. All rights reserved.")
	fmt.Println("GitHub: https://github.com/pai801/myapi")
	fmt.Println("Usage: MyAPI --port <port> [--log-dir <log directory>] [--version] [--help]")
}

func Init() {
	flag.Parse()

	if *PrintVersion {
		fmt.Println(Version)
		os.Exit(0)
	}

	if *PrintHelp {
		printHelp()
		os.Exit(0)
	}

	if os.Getenv("SESSION_SECRET") != "" {
		if os.Getenv("SESSION_SECRET") == "random_string" {
			logger.Log.Errorf("SESSION_SECRET is set to an example value, please change it to a random string.")
		} else {
			config.SessionSecret = os.Getenv("SESSION_SECRET")
		}
	}
	if os.Getenv("JWT_SECRET") != "" {
		config.JWTSecret = os.Getenv("JWT_SECRET")
	} else if os.Getenv("SESSION_SECRET") != "" {
		config.JWTSecret = config.SessionSecret
	}
	if os.Getenv("SQLITE_PATH") != "" {
		SQLitePath = os.Getenv("SQLITE_PATH")
	}
	if *LogDir != "" {
		var err error
		*LogDir, err = filepath.Abs(*LogDir)
		if err != nil {
			log.Fatal(err)
		}
		if _, err := os.Stat(*LogDir); os.IsNotExist(err) {
			err = os.Mkdir(*LogDir, 0777)
			if err != nil {
				log.Fatal(err)
			}
		}
		logger.LogDir = *LogDir
	}
}
