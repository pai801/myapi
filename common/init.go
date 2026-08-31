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
	} else {
		logger.Log.Warnf("SESSION_SECRET is not set, using a random value per start; sessions will be invalidated on restart or across replicas, set SESSION_SECRET to keep them stable.")
	}
	if os.Getenv("JWT_SECRET") != "" {
		config.JWTSecret = os.Getenv("JWT_SECRET")
	} else if os.Getenv("SESSION_SECRET") != "" {
		config.JWTSecret = config.SessionSecret
	}
	// 默认密钥是公开已知的，可被用来自签任意角色的 JWT 直接获得 root 权限，必须拒绝启动
	if config.JWTSecret == "myapi-jwt-secret" {
		logger.Log.Fatalf("JWT_SECRET is not set: refusing to start with the default secret, which allows anyone to forge admin JWTs. Set the JWT_SECRET environment variable, e.g. export JWT_SECRET=$(openssl rand -hex 32)")
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
