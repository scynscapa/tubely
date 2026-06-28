package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/database"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<30)
	err := r.ParseMultipartForm(1 << 30)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			respondWithError(w, http.StatusRequestEntityTooLarge, "Request body is too large", err)
			return
		}
		respondWithError(w, http.StatusBadRequest, "Bad request", err)
		return
	}

	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't find video", err)
		return
	}
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Not authorized to update this video", nil)
		return
	}

	file, fileData, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer file.Close()

	mediaType, _, err := mime.ParseMediaType(fileData.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid Content-Type", err)
		return
	}
	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Invalid file type", nil)
		return
	}

	tempFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to create temp file", err)
		return
	}
	defer os.Remove(tempFile.Name())
	defer tempFile.Close()

	io.Copy(tempFile, file)
	tempFile.Seek(0, io.SeekStart)

	ratio, err := cfg.getVideoAspectRatio(tempFile.Name())

	assetPath := getAssetPath(mediaType)
	assetPathRatio := fmt.Sprintf("%s/%s", ratio, assetPath)

	processedFilePath, err := processVideoForFastStart(tempFile.Name())
	processedFile, err := os.Open(processedFilePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to open processed file", err)
		return
	}
	defer processedFile.Close()

	_, err = cfg.s3Client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket:      aws.String(cfg.s3Bucket),
		Key:         aws.String(assetPathRatio),
		Body:        processedFile,
		ContentType: &mediaType,
	})
	if err != nil {
		log.Fatalf("failed to upload object, %v", err)
	}

	// url := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, assetPathRatio)
	url := fmt.Sprintf("%s,%s", cfg.s3Bucket, assetPathRatio)
	video.VideoURL = &url

	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video", err)
		return
	}

	returnVideo, err := cfg.dbVideoToSignedVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to process signed video", err)
		return
	}

	respondWithJSON(w, http.StatusOK, returnVideo)
}

type Streams struct {
	Streams []Stats `json:"streams"`
}

type Stats struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

func (cfg *apiConfig) getVideoAspectRatio(filePath string) (string, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	var buffer bytes.Buffer
	cmd.Stdout = &buffer

	err := cmd.Run()
	if err != nil {
		log.Fatal(err)
	}

	var data Streams

	err = json.Unmarshal(buffer.Bytes(), &data)
	if err != nil {
		return "", fmt.Errorf("Could not unmarshal json: %s", err)
	}

	height := data.Streams[0].Height
	width := data.Streams[0].Width

	ratio := float64(width) / float64(height)
	landscape := float64(16) / 9
	portrait := float64(9) / 16

	var ratioString string

	switch {
	case ratio >= landscape-0.1 && ratio <= landscape+0.1:
		ratioString = "landscape"
	case ratio >= portrait-0.1 && ratio <= portrait+0.1:
		ratioString = "portrait"
	default:
		ratioString = "other"
	}

	return ratioString, nil
}

func processVideoForFastStart(filePath string) (string, error) {
	outputPath := fmt.Sprintf("%s.processing", filePath)

	cmd := exec.Command("ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", outputPath)

	err := cmd.Run()
	if err != nil {
		log.Fatal(err)
	}

	return outputPath, nil
}

func generatePresignedURL(s3Client *s3.Client, bucket, key string, expireTime time.Duration) (string, error) {
	presignClient := s3.NewPresignClient(s3Client)

	presignObject, err := presignClient.PresignGetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(expireTime))
	if err != nil {
		log.Fatalf("Failed to presign request, %v", err)
	}

	return presignObject.URL, nil
}

func (cfg *apiConfig) dbVideoToSignedVideo(video database.Video) (database.Video, error) {
	if video.VideoURL == nil {
		return video, nil
	}

	videoSplit := strings.Split(*video.VideoURL, ",")
	if len(videoSplit) < 2 {
		err := fmt.Errorf("VideoURL is not formatted correctly")
		return video, err
	}
	bucket := videoSplit[0]
	key := videoSplit[1]

	url, err := generatePresignedURL(cfg.s3Client, bucket, key, 10*time.Second)
	if err != nil {
		return video, err
	}
	video.VideoURL = &url

	return video, nil
}
