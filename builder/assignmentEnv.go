package builder

import (
	"assignment-exec/image-builder/configurations"
	"assignment-exec/image-builder/constants"
	"assignment-exec/image-builder/environment"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/pkg/errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
)

type baseEnvImage interface {
	verifyAndWriteInstructions() error
	build() error
	publish() error
	resetDockerfileData()
	deleteDockerfile() error
	undoBuild() error
}

type assignmentEnvironment struct {
	DockerfileInstructions bytes.Buffer
	ImgBuildConfig         *imageBuildConfig
	AssgnEnvConfig         *configurations.AssignmentEnvConfig
	ImageExists            bool
}

type assignmentEnvironmentOption func(*assignmentEnvironment) error

func newAssignmentEnvironment(options ...assignmentEnvironmentOption) (*assignmentEnvironment, error) {
	assgnEnv := &assignmentEnvironment{}
	for _, opt := range options {
		if err := opt(assgnEnv); err != nil {
			return nil, errors.Wrap(err, "failed to create assignmentEnvironment instance")
		}
	}
	return assgnEnv, nil
}

func withImageBuildCfg(imgBuildCfg *imageBuildConfig) assignmentEnvironmentOption {
	return func(assgnEnv *assignmentEnvironment) error {
		if imgBuildCfg == nil {
			return errors.New("image build config instance not provided")
		}
		assgnEnv.ImgBuildConfig = imgBuildCfg
		return nil

	}
}

func withAssgnEnvConfig(assignCfgs *configurations.AssignmentEnvConfig) assignmentEnvironmentOption {
	return func(assgnEnv *assignmentEnvironment) error {
		if assignCfgs == nil {
			return errors.New("assignment environment configurations not provided")
		}
		assgnEnv.AssgnEnvConfig = assignCfgs
		return nil

	}
}

func (assgnEnv *assignmentEnvironment) verifyAndWriteInstructions() error {

	// Verify whether language image is present in registry.
	if err := assgnEnv.verifyLanguage(); err != nil {
		// If no then write the instructions from base image.
		assgnEnv.writeFromBaseImage()
	} else {
		if len(assgnEnv.AssgnEnvConfig.Deps.Libraries) > 0 {
			// Else write the instructions from dependencies.
			assgnEnv.writeFromDependencies()
		}
	}

	if assgnEnv.DockerfileInstructions.Len() <= 0 {
		assgnEnv.ImageExists = true
	}
	return nil
}

func (assgnEnv *assignmentEnvironment) verifyLanguage() error {
	// Check whether the language image is available on docker hub.
	backgroundContext := context.Background()
	dockerClient, err := client.NewEnvClient()
	if err != nil {
		return err
	}
	username, usernameFound := os.LookupEnv(environment.DockerAuthUsername)
	if !usernameFound {
		return errors.New("environment variable for username not set")
	}

	response, err := dockerClient.ImageSearch(backgroundContext, username, types.ImageSearchOptions{
		Limit: 25})

	if err != nil {
		return err
	}
	langImageFound := false
	for _, result := range response {
		if strings.Contains(assgnEnv.ImgBuildConfig.imageTag, result.Name) {
			langImageFound = true
		}
	}
	if !langImageFound {
		return errors.New("language image not found on docker hub")
	}

	return nil
}

func (assgnEnv *assignmentEnvironment) writeFromBaseImage() {
	var instructions []string
	instructions = append(instructions, assgnEnv.AssgnEnvConfig.GetInstruction())

	var libraryNames []string
	for lib := range assgnEnv.AssgnEnvConfig.Deps.Libraries {
		// Append library names to an array.
		libraryNames = append(libraryNames, lib)
	}

	// Generate the image tag.
	assgnEnv.ImgBuildConfig.imageTag = strings.Join([]string{assgnEnv.ImgBuildConfig.imageTag,
		strings.Join(libraryNames, "-")}, "-")
	assgnEnv.DockerfileInstructions.WriteString(strings.Join(instructions, "\n"))
}

func (assgnEnv *assignmentEnvironment) writeFromDependencies() {
	var instructions []string

	// FROM instruction.
	instructions = append(instructions, fmt.Sprintf("FROM %s", assgnEnv.ImgBuildConfig.imageTag))
	// COPY instruction.
	instructions = append(instructions, fmt.Sprintf("COPY . /"+constants.CodeRunnerDir))

	var libraryNames []string
	for lib, installCmd := range assgnEnv.AssgnEnvConfig.Deps.Libraries {
		// Append library names to an array.
		libraryNames = append(libraryNames, lib)

		// RUN instruction.
		instructions = append(instructions, "RUN "+installCmd.GetInstruction()+" "+lib)
	}

	// Generate the image tag.
	assgnEnv.ImgBuildConfig.imageTag = strings.Join([]string{assgnEnv.ImgBuildConfig.imageTag,
		strings.Join(libraryNames, "-")}, "-")
	assgnEnv.DockerfileInstructions.WriteString(strings.Join(instructions, "\n"))
}

func (assgnEnv *assignmentEnvironment) writeToDockerfile() error {
	if !assgnEnv.ImageExists {

		file, err := os.Create(assgnEnv.ImgBuildConfig.dockerfileLoc)
		defer func() {
			err = file.Close()
			if err != nil {
				log.Println("error while closing the created Dockerfile", err)
				return
			}
		}()

		if err != nil {
			return err
		}
		_, err = file.WriteString(assgnEnv.DockerfileInstructions.String())
		return err
	}
	return nil
}

func (assgnEnv *assignmentEnvironment) build() error {

	if !assgnEnv.ImageExists {
		backgroundContext := context.Background()
		dockerClient, err := client.NewEnvClient()
		if err != nil {
			return errors.Wrap(err, "error in creating a docker client")
		}

		// Create a build context tar for the image.
		// build Context is the current working directory and where the Dockerfile is assumed to be located.
		// [cite: https://docs.docker.com/develop/develop-images/dockerfile_best-practices/].
		dockerfileLoc := filepath.Base(assgnEnv.ImgBuildConfig.dockerfileLoc)

		dockerBuildContext, err := assgnEnv.ImgBuildConfig.getDockerBuildContextTar()
		if err != nil {
			return err
		}

		response, err := dockerClient.ImageBuild(
			backgroundContext,
			dockerBuildContext,
			types.ImageBuildOptions{
				Dockerfile: dockerfileLoc,
				Tags:       []string{assgnEnv.ImgBuildConfig.imageTag}})
		if err != nil {
			return errors.Wrap(err, "error in building docker image")
		}
		defer func() {
			err := response.Body.Close()
			if err != nil {
				log.Println(err)
				return
			}
		}()

		_, err = io.Copy(os.Stdout, response.Body)
		if err != nil {
			return errors.Wrap(err, "error in reading image build response")
		}

		return err
	} else {
		return assgnEnv.pullImage()
	}
}

func (assgnEnv *assignmentEnvironment) publish() error {
	if !assgnEnv.ImageExists && assgnEnv.ImgBuildConfig.publishImage {
		return assgnEnv.pushImage()
	}
	return nil
}

func (assgnEnv *assignmentEnvironment) pullImage() error {
	backgroundContext := context.Background()
	dockerClient, err := client.NewEnvClient()
	if err != nil {
		return errors.Wrap(err, "error in creating new docker client")
	}
	authConfig := types.AuthConfig{
		Username: assgnEnv.ImgBuildConfig.authData.Username,
		Password: assgnEnv.ImgBuildConfig.authData.Password,
	}
	authJson, err := json.Marshal(authConfig)
	if err != nil {
		return errors.Wrap(err, "error in encoding authConfig")
	}

	authString := base64.URLEncoding.EncodeToString(authJson)
	imageString := fmt.Sprintf("%s/%s", constants.DockerIO, assgnEnv.ImgBuildConfig.imageTag)
	response, err := dockerClient.ImagePull(backgroundContext, imageString, types.ImagePullOptions{
		RegistryAuth: authString,
	})

	if err != nil {
		return errors.Wrap(err, "error in pulling image from hub")
	}
	_, err = io.Copy(os.Stdout, response)
	if err != nil {
		return errors.Wrap(err, "error in reading image pull response")
	}
	defer func() {
		err = response.Close()
		if err != nil {
			log.Println(err)
			return
		}
	}()

	return nil

}
func (assgnEnv *assignmentEnvironment) pushImage() error {
	backgroundContext := context.Background()
	dockerClient, err := client.NewEnvClient()
	if err != nil {
		return errors.Wrap(err, "error in creating new docker client")
	}

	authConfig := types.AuthConfig{
		Username: assgnEnv.ImgBuildConfig.authData.Username,
		Password: assgnEnv.ImgBuildConfig.authData.Password,
	}
	authJson, err := json.Marshal(authConfig)
	if err != nil {
		return errors.Wrap(err, "error in encoding authConfig")
	}

	authString := base64.URLEncoding.EncodeToString(authJson)

	imageString := fmt.Sprintf("%s/%s", constants.DockerIO, assgnEnv.ImgBuildConfig.imageTag)

	response, err := dockerClient.ImagePush(backgroundContext, imageString, types.ImagePushOptions{
		RegistryAuth: authString,
	})
	if err != nil {
		return errors.Wrap(err, "error in pushing image to hub")
	}
	_, err = io.Copy(os.Stdout, response)
	if err != nil {
		return errors.Wrap(err, "error in reading image push response")
	}
	defer func() {
		err = response.Close()
		if err != nil {
			log.Println(err)
			return
		}
	}()
	return nil
}

func (assgnEnv *assignmentEnvironment) resetDockerfileData() {
	// Clear the dockerfile data.
	assgnEnv.DockerfileInstructions.Reset()
}

func (assgnEnv *assignmentEnvironment) deleteDockerfile() error {
	// Delete the created Dockerfile.
	_, err := os.Stat(assgnEnv.ImgBuildConfig.dockerfileLoc)
	if err == nil {
		return os.Remove(assgnEnv.ImgBuildConfig.dockerfileLoc)
	}
	return nil
}

func (assgnEnv *assignmentEnvironment) undoBuild() error {
	// Delete the built image.
	backgroundContext := context.Background()
	dockerClient, err := client.NewEnvClient()
	if err != nil {
		return err
	}

	_, err = dockerClient.ImageRemove(backgroundContext, assgnEnv.ImgBuildConfig.imageTag, types.ImageRemoveOptions{
		Force: true})

	return err
}
