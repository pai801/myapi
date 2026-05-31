package common

import (
	"time"

	"github.com/golang-jwt/jwt"
	"github.com/songquanpeng/one-api/common/config"
)

type JWTClaims struct {
	UserId   int    `json:"user_id"`
	Username string `json:"username"`
	Role     int    `json:"role"`
	Status   int    `json:"status"`
	jwt.StandardClaims
}

func GenerateJWT(userId int, username string, role int, status int) (string, error) {
	claims := JWTClaims{
		UserId:   userId,
		Username: username,
		Role:     role,
		Status:   status,
		StandardClaims: jwt.StandardClaims{
			ExpiresAt: time.Now().Add(time.Duration(config.JWTExpiresIn) * time.Second).Unix(),
			IssuedAt:  time.Now().Unix(),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(config.JWTSecret))
}

func ParseJWT(tokenString string) (*JWTClaims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &JWTClaims{}, func(token *jwt.Token) (interface{}, error) {
		return []byte(config.JWTSecret), nil
	})
	if err != nil {
		return nil, err
	}
	if claims, ok := token.Claims.(*JWTClaims); ok && token.Valid {
		return claims, nil
	}
	return nil, jwt.ErrSignatureInvalid
}
