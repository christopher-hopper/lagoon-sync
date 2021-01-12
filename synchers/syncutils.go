package synchers

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"strings"
	"text/template"

	"github.com/amazeeio/lagoon-sync/assets"
	"gopkg.in/yaml.v2"
)

var shellToUse = "sh"

// UnmarshallLagoonYamlToLagoonSyncStructure will take a bytestream and return a fully parsed lagoon sync config structure
func UnmarshallLagoonYamlToLagoonSyncStructure(data []byte) (SyncherConfigRoot, error) {
	lagoonConfig := SyncherConfigRoot{}
	err := yaml.Unmarshal(data, &lagoonConfig)

	if err != nil {
		return SyncherConfigRoot{}, errors.New("Unable to parse lagoon config yaml setup")
	}
	return lagoonConfig, nil
}

func RunSyncProcess(sourceEnvironment Environment, targetEnvironment Environment, lagoonSyncer Syncer, syncerType string, dryRun bool, verboseSSH bool) error {
	var err error
	// sourceEnvironment, err = RunPrerequisiteCommand(sourceEnvironment, lagoonSyncer, syncerType, dryRun, verboseSSH)
	//sourceRsyncPath := sourceEnvironment.RsyncPath
	if err != nil {
		return err
	}

	err = SyncRunSourceCommand(sourceEnvironment, lagoonSyncer, dryRun, verboseSSH)
	if err != nil {
		_ = SyncCleanUp(sourceEnvironment, lagoonSyncer, dryRun, verboseSSH)
		return err
	}

	// targetEnvironment, err = RunPrerequisiteCommand(targetEnvironment, lagoonSyncer, syncerType, dryRun, verboseSSH)
	//targetRsyncPath := targetEnvironment.RsyncPath
	if err != nil {
		return err
	}
	err = SyncRunTransfer(sourceEnvironment, targetEnvironment, lagoonSyncer, dryRun, verboseSSH)
	if err != nil {
		_ = SyncCleanUp(sourceEnvironment, lagoonSyncer, dryRun, verboseSSH)
		return err
	}

	err = SyncRunTargetCommand(targetEnvironment, lagoonSyncer, dryRun, verboseSSH)
	if err != nil {
		_ = SyncCleanUp(sourceEnvironment, lagoonSyncer, dryRun, verboseSSH)
		_ = SyncCleanUp(targetEnvironment, lagoonSyncer, dryRun, verboseSSH)
		return err
	}

	_ = SyncCleanUp(sourceEnvironment, lagoonSyncer, dryRun, verboseSSH)
	_ = SyncCleanUp(targetEnvironment, lagoonSyncer, dryRun, verboseSSH)

	return nil
}

func SyncRunSourceCommand(remoteEnvironment Environment, syncer Syncer, dryRun bool, verboseSSH bool) error {

	log.Printf("Beginning export on source environment (%s)", remoteEnvironment.EnvironmentName)

	if syncer.GetRemoteCommand(remoteEnvironment).NoOp {
		log.Printf("Found No Op for environment %s - skipping step", remoteEnvironment.EnvironmentName)
		return nil
	}

	var execString string

	command, commandErr := syncer.GetRemoteCommand(remoteEnvironment).GetCommand()
	if commandErr != nil {
		return commandErr
	}

	if remoteEnvironment.EnvironmentName == LOCAL_ENVIRONMENT_NAME {
		execString = command
	} else {
		execString = generateRemoteCommand(remoteEnvironment, command, verboseSSH)
	}

	log.Printf("Running the following for source :- %s", execString)

	if !dryRun {
		err, response, errstring := Shellout(execString)
		if err != nil {
			fmt.Println(errstring)
			return err
		}
		if response != "" {
			fmt.Println(response)
		}
	}
	return nil
}

func SyncRunTransfer(sourceEnvironment Environment, targetEnvironment Environment, syncer Syncer, dryRun bool, verboseSSH bool) error {
	log.Println("Beginning file transfer logic")

	// If we're transferring to the same resource, we can skip this whole process.
	if sourceEnvironment.EnvironmentName == targetEnvironment.EnvironmentName {
		log.Println("Source and target environments are the same, skipping transfer")
		return nil
	}

	// For now, we assert that _one_ of the environments _has_ to be local
	executeRsyncRemotelyOnTarget := false
	if sourceEnvironment.EnvironmentName != LOCAL_ENVIRONMENT_NAME && targetEnvironment.EnvironmentName != LOCAL_ENVIRONMENT_NAME {
		//TODO: if we have multiple remotes, we need to treat the target environment as local, and run the rysync from there ...
		log.Println("Note - since we're syncing across two remote systems, we're pulling the files _to_ the target")
		executeRsyncRemotelyOnTarget = true
	}

	if sourceEnvironment.EnvironmentName == LOCAL_ENVIRONMENT_NAME && targetEnvironment.EnvironmentName == LOCAL_ENVIRONMENT_NAME {
		return errors.New("In order to rsync, at least _one_ of the environments must be remote")
	}

	sourceEnvironmentName := syncer.GetTransferResource(sourceEnvironment).Name
	if syncer.GetTransferResource(sourceEnvironment).IsDirectory == true {
		sourceEnvironmentName += "/"
	}

	// lagoonRsyncService keeps track of precisely where we're going to be rsyncing from.
	lagoonRsyncService := "cli"
	// rsyncRemoteSystemUsername is used by the rsync command to set up the ssh tunnel
	rsyncRemoteSystemUsername := ""

	if sourceEnvironment.EnvironmentName != LOCAL_ENVIRONMENT_NAME {
		//sourceEnvironmentName = fmt.Sprintf("%s@ssh.lagoon.amazeeio.cloud:%s", sourceEnvironment.getOpenshiftProjectName(), sourceEnvironmentName)
		sourceEnvironmentName = fmt.Sprintf(":%s", sourceEnvironmentName)
		rsyncRemoteSystemUsername = sourceEnvironment.getOpenshiftProjectName()
		if sourceEnvironment.ServiceName != "" {
			lagoonRsyncService = sourceEnvironment.ServiceName
		}
	}

	targetEnvironmentName := syncer.GetTransferResource(targetEnvironment).Name
	if targetEnvironment.EnvironmentName != LOCAL_ENVIRONMENT_NAME && executeRsyncRemotelyOnTarget == false {
		//targetEnvironmentName = fmt.Sprintf("%s@ssh.lagoon.amazeeio.cloud:%s", targetEnvironment.getOpenshiftProjectName(), targetEnvironmentName)
		targetEnvironmentName = fmt.Sprintf(":%s", targetEnvironmentName)
		rsyncRemoteSystemUsername = targetEnvironment.getOpenshiftProjectName()
		if targetEnvironment.ServiceName != "" {
			lagoonRsyncService = targetEnvironment.ServiceName
		}
	}

	syncExcludes := " "
	for _, e := range syncer.GetTransferResource(sourceEnvironment).ExcludeResources {
		syncExcludes += fmt.Sprintf("--exclude=%v ", e)
	}

	verboseSSHArgument := ""
	if verboseSSH {
		verboseSSHArgument = "-v"
	}

	execString := fmt.Sprintf("%v --rsync-path=%v -e \"ssh %v -o LogLevel=ERROR -o UserKnownHostsFile=/dev/null -o StrictHostKeyChecking=no -p 32222 -l %v ssh.lagoon.amazeeio.cloud service=%v\" %v -a %s %s",
		"rsync",
		"rsync",
		verboseSSHArgument,
		rsyncRemoteSystemUsername,
		lagoonRsyncService,
		syncExcludes,
		sourceEnvironmentName,
		targetEnvironmentName)

	if executeRsyncRemotelyOnTarget {
		execString = generateRemoteCommand(targetEnvironment, execString, verboseSSH)
	}

	log.Printf("Running the following for target (%s) :- %s", targetEnvironment.EnvironmentName, execString)

	if !dryRun {
		if err, _, errstring := Shellout(execString); err != nil {
			log.Println(errstring)
			return err
		}
	}

	return nil
}

func SyncRunTargetCommand(targetEnvironment Environment, syncer Syncer, dryRun bool, verboseSSH bool) error {

	log.Printf("Beginning import on target environment (%s)", targetEnvironment.EnvironmentName)

	if syncer.GetRemoteCommand(targetEnvironment).NoOp {
		log.Printf("Found No Op for environment %s - skipping step", targetEnvironment.EnvironmentName)
		return nil
	}

	var execString string
	command, commandErr := syncer.GetLocalCommand(targetEnvironment).GetCommand()
	if commandErr != nil {
		return commandErr
	}

	if targetEnvironment.EnvironmentName == LOCAL_ENVIRONMENT_NAME {
		execString = command
	} else {
		execString = generateRemoteCommand(targetEnvironment, command, verboseSSH)
	}

	log.Printf("Running the following for target (%s) :- %s", targetEnvironment.EnvironmentName, execString)
	if !dryRun {
		err, _, errstring := Shellout(execString)
		if err != nil {
			fmt.Println(errstring)
			return err
		}
	}

	return nil
}

func SyncCleanUp(environment Environment, syncer Syncer, dryRun bool, verboseSSH bool) error {
	transferResouce := syncer.GetTransferResource(environment)

	if transferResouce.SkipCleanup == true {
		log.Printf("Skipping cleanup for %v on %v environment", transferResouce.Name, environment.EnvironmentName)
		return nil
	}

	transferResourceName := transferResouce.Name

	execString := fmt.Sprintf("rm -r %s", transferResourceName)

	if environment.EnvironmentName != LOCAL_ENVIRONMENT_NAME {
		execString = generateRemoteCommand(environment, execString, verboseSSH)
	}

	log.Printf("Beginning resource cleanup on %s", environment.EnvironmentName)
	log.Printf("Running the following: %s", execString)

	if !dryRun {
		err, _, errstring := Shellout(execString)

		if err != nil {
			fmt.Println(errstring)
			return err
		}
	}

	return nil
}

func generateNoOpSyncCommand() SyncCommand {
	return SyncCommand{
		NoOp: true,
	}
}

func generateSyncCommand(commandString string, substitutions map[string]interface{}) SyncCommand {
	return SyncCommand{
		command:       commandString,
		substitutions: substitutions,
		NoOp:          false,
	}
}

func (c SyncCommand) GetCommand() (string, error) {
	if c.NoOp == true {
		return "", errors.New("The command is marked as NoOp(eration) and does not generate a string")
	}
	templ, err := template.New("Command Template").Parse(c.command)
	if err != nil {
		return "", err
	}
	var output bytes.Buffer
	if err := templ.Execute(&output, c.substitutions); err != nil {
		return "", err
	}
	return output.String(), nil
}

func generateRemoteCommand(remoteEnvironment Environment, command string, verboseSSH bool) string {
	verbose := ""
	if verboseSSH {
		verbose = "-v"
	}

	serviceArgument := ""
	if remoteEnvironment.ServiceName != "" {
		if remoteEnvironment.ServiceName == "mongodb" {
			shellToUse = "sh"
		}
		serviceArgument = fmt.Sprintf("service=%v", remoteEnvironment.ServiceName)
	}

	return fmt.Sprintf("ssh %s -tt -o \"UserKnownHostsFile=/dev/null\" -o \"StrictHostKeyChecking=no\" -p 32222 %v@ssh.lagoon.amazeeio.cloud %v '%v'",
		verbose, remoteEnvironment.getOpenshiftProjectName(), serviceArgument, command)
}

func Shellout(command string) (error, string, string) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd := exec.Command(shellToUse, "-c", command)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return err, stdout.String(), stderr.String()
}

func getEnv(key string, defaultVal string) string {
	if _, exists := os.LookupEnv(key); exists {
		return key
	}
	return defaultVal
}

var RsyncAssetPath = "/tmp/rsync"

// will add bundled rsync onto environment and return the new rsync path as string
func createRsync(environment Environment, syncer Syncer, lagoonSyncVersion string) (string, error) {
	if environment.EnvironmentName == LOCAL_ENVIRONMENT_NAME {
		log.Printf("%v environment doesn't have rsync", environment.EnvironmentName)
	}

	log.Printf("Downloading rsync asset on %v", environment.EnvironmentName)

	environmentName := syncer.GetTransferResource(environment).Name
	if syncer.GetTransferResource(environment).IsDirectory == true {
		environmentName += "/"
	}

	// Create rsync asset
	RsyncAssetPath, err := createRsyncAssetFromBytes(lagoonSyncVersion)
	if err != nil {
		log.Println(err)
	}

	rsyncDestinationPath := fmt.Sprintf("%vlagoon_sync_rsync_%v", "/tmp/", strings.ReplaceAll(lagoonSyncVersion, ".", "_"))

	environment.RsyncLocalPath = rsyncDestinationPath
	environment.RsyncPath = RsyncAssetPath

	//If local we bail out here
	if environment.EnvironmentName == LOCAL_ENVIRONMENT_NAME {
		return rsyncDestinationPath, nil
	}

	var execString string
	command := fmt.Sprintf("'cat > %v && chmod +x %v' < %s",
		rsyncDestinationPath,
		rsyncDestinationPath,
		rsyncDestinationPath,
	)

	if environment.EnvironmentName == LOCAL_ENVIRONMENT_NAME {
		execString = command
	} else {
		serviceArgument := ""
		if environment.ServiceName != "" {
			if environment.ServiceName == "mongodb" {
				shellToUse = "sh"
			}
			serviceArgument = fmt.Sprintf("service=%v", environment.ServiceName)

		}

		execString = fmt.Sprintf("ssh -t -o \"UserKnownHostsFile=/dev/null\" -o \"StrictHostKeyChecking=no\" -p 32222 %v@ssh.lagoon.amazeeio.cloud %v %v",
			environment.getOpenshiftProjectName(), serviceArgument, command)
	}

	log.Printf("Running the following for:- %s", execString)

	if err, _, errstring := Shellout(execString); err != nil {
		log.Println(errstring)
		return "", err
	}

	return rsyncDestinationPath, nil
}

func createRsyncAssetFromBytes(lagoonSyncVersion string) (string, error) {
	tempRsyncPath := "/tmp/rsync"
	err := ioutil.WriteFile(tempRsyncPath, assets.GetRSYNC(), 0774)
	if err != nil {
		log.Fatal(err)
	}

	if lagoonSyncVersion == "" {
		log.Print("No lagoon-sync version set")
	}

	// Test running local rsync binary (won't run on darwin OS)
	// localRsyncCommand := exec.Command(tempRsyncPath", "--version")
	// stdout, err := localRsyncCommand.StdoutPipe()
	// if err != nil {
	// 	log.Fatal(err)
	// }
	// if err := localRsyncCommand.Start(); err != nil {
	// 	log.Fatal(err)
	// }

	// r := bufio.NewReader(stdout)
	// b, err := r.ReadBytes('\n')
	// if err != nil {
	// 	log.Fatal(err)
	// }
	// fmt.Println("Local rsync ran:", string(b))

	// Rename rsync binary with latest lagoon version appended.
	versionedRsyncPath := fmt.Sprintf("%vlagoon_sync_rsync_%v", "/tmp/", strings.ReplaceAll(lagoonSyncVersion, ".", "_"))
	cpRsyncCmd := fmt.Sprintf("cp %s %s",
		tempRsyncPath,
		versionedRsyncPath,
	)

	if err, _, errstring := Shellout(cpRsyncCmd); err != nil {
		log.Println(errstring)
		return "", err
	}

	removeLocalRsyncCopyExecString := fmt.Sprintf("rm -rf %v", tempRsyncPath)
	log.Printf("Removing temp rsync binary: %v", removeLocalRsyncCopyExecString)
	if err, _, errstring := Shellout(removeLocalRsyncCopyExecString); err != nil {
		log.Println(errstring)
		return "", err
	}

	return versionedRsyncPath, nil
}
