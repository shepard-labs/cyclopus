package avatar

import (
	"crypto/rand"
	"errors"
	"math/big"
)

const DefaultKey = "1ba2e39a64"

var Keys = []string{
	"1ba2e39a64", "21d7a04043", "27663a0c89", "2b7c2b63eb", "3049b7f308",
	"32f58ce69a", "4b86f28969", "5b03ce5dd6", "5f0d989fa0", "63ef474ed7",
	"6421d2b952", "6c7b9a5fc3", "83ba7d2ccc", "8fc61c2b7c", "9360e13321",
	"9713ff673c", "a1f20ef39d", "aaceb297d2", "b433ec9e76", "b45893ed6c",
	"bee3d66767", "c7347a4996", "d00b8e978a", "d8325d0118", "daec39eb78",
	"e5e4b7fad8", "f0745d306e", "f524649182", "fa79c9c5a5", "fec9e5954b",
}

func Valid(key string) bool {
	for _, candidate := range Keys {
		if key == candidate {
			return true
		}
	}
	return false
}

func RandomKey() (string, error) {
	if len(Keys) == 0 {
		return "", errors.New("avatar set is empty")
	}
	index, err := rand.Int(rand.Reader, big.NewInt(int64(len(Keys))))
	if err != nil {
		return "", err
	}
	return Keys[index.Int64()], nil
}

func URL(key string) string {
	return "/avatars/" + key + ".svg"
}
