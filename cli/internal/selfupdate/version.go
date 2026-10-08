package selfupdate

import (
	"errors"
	"strconv"
	"strings"
)

var ErrInvalidRelease = errors.New("signed release is invalid")

func validVersion(value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) != 4 || len(parts[0]) != 4 || len(parts[1]) != 2 || len(parts[2]) != 2 || parts[3] == "" || len(parts[3]) > 4 || len(parts[3]) > 1 && parts[3][0] == '0' {
		return false
	}
	for _, part := range parts {
		for _, character := range part {
			if character < '0' || character > '9' {
				return false
			}
		}
	}
	return true
}

func CompareVersions(left, right string) (int, error) {
	if !validVersion(left) || !validVersion(right) {
		return 0, ErrInvalidRelease
	}
	leftParts, rightParts := strings.Split(left, "."), strings.Split(right, ".")
	for index := range leftParts {
		leftValue, _ := strconv.Atoi(leftParts[index])
		rightValue, _ := strconv.Atoi(rightParts[index])
		if leftValue < rightValue {
			return -1, nil
		}
		if leftValue > rightValue {
			return 1, nil
		}
	}
	return 0, nil
}
