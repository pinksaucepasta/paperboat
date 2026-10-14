package containers

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

type Images struct {
	Relay  string `json:"relay,omitempty"`
	Tunnel string `json:"tunnel,omitempty"`
}

var imagePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/@-]{0,511}$`)

func (i Images) Image(capability string) (string, error) {
	var image string
	switch capability {
	case "relay":
		image = i.Relay
	case "tunnel":
		image = i.Tunnel
	default:
		return "", errors.New("unsupported runtime capability")
	}
	if !imagePattern.MatchString(image) {
		return "", errors.New("selected runtime image is missing or invalid")
	}
	return image, nil
}

func SaveImages(dir string, images Images) error {
	if !filepath.IsAbs(dir) {
		return errors.New("installation directory must be absolute")
	}
	for _, image := range []string{images.Relay, images.Tunnel} {
		if image != "" && !imagePattern.MatchString(image) {
			return errors.New("runtime image is invalid")
		}
	}
	b, err := json.Marshal(images)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".images-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(b)
	}
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(file.Name(), filepath.Join(dir, "images.json"))
}

func LoadImages(dir string) (Images, error) {
	var images Images
	path := filepath.Join(dir, "images.json")
	info, err := os.Lstat(path)
	if err != nil {
		return images, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 16384 {
		return images, errors.New("runtime image configuration must be a bounded owner-only regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return images, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 16385))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&images); err != nil {
		return images, errors.New("runtime image configuration is invalid")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return images, errors.New("runtime image configuration has trailing data")
	}
	return images, nil
}
