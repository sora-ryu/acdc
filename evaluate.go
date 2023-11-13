package main

import (
	"acdc/mbc3"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

type Exec struct {
	Path    string `json:"Path"`
	Version string `json:"Version"`
	Valid   bool   `json:"Valid"`
}

func NewExec() *Exec {
	return &Exec{}
}

type EvalStatus struct {
	ID          int    `json:"ID"`
	State       string `json:"State"`
	SimProgress int    `json:"SimProgress"`
	LinProgress int    `json:"LinProgress"`
	Error       string `json:"Error"`
}

type EvalLog struct {
	ID   int    `json:"ID"`
	Line string `json:"Line"`
}

var EvalCancel context.CancelCauseFunc = func(_ error) {}

func (p *Project) EvaluateLinearization(ctx context.Context, c *Case, op *Condition, caseDir string) error {

	//--------------------------------------------------------------------------
	// Prepare input files
	//--------------------------------------------------------------------------

	// Create a local copy of the files so modifications don't affect the original
	files, err := p.Model.Files.Copy()
	if err != nil {
		return err
	}

	// Get number of linearization steps, set LinTimes
	if len(files.Main) > 0 {

		// The file writing code assumes that NLinTimes is governed by the
		// length of LinTimes which is only true if CalcSteady==false.
		// If CalcSteady is true, the size of LinTimes must be changed
		// to match NLinTimes
		if files.Main[0].CalcSteady.Value {
			files.Main[0].LinTimes.Value = make([]float64, files.Main[0].NLinTimes.Value)
		}

	} else {
		return fmt.Errorf("no Main files were imported")
	}

	// If ElastoDyn files present modify for operating point conditions
	if len(files.ElastoDyn) > 0 {
		files.ElastoDyn[0].BlPitch1.Value = op.BladePitch
		files.ElastoDyn[0].BlPitch2.Value = op.BladePitch
		files.ElastoDyn[0].BlPitch3.Value = op.BladePitch
		files.ElastoDyn[0].RotSpeed.Value = op.RotorSpeed
	} else {
		return fmt.Errorf("no ElastoDyn files were imported")
	}

	// If BeamDyn files are present, set RotateStates to true
	for i := range files.BeamDyn {
		files.BeamDyn[i].RotStates.Value = true
	}

	// If case includes aero and wind speed is nonzero
	if c.IncludeAero && op.WindSpeed > 0 {

		// Set flag to use InflowWind
		files.Main[0].CompInflow.Value = 1

		// Set flag to use AeroDyn or return error
		if len(files.AeroDyn) > 0 {
			files.Main[0].CompAero.Value = 2
		} else if len(files.AeroDyn14) > 0 {
			files.Main[0].CompAero.Value = 1
		} else {
			return fmt.Errorf("no Aero files were imported")
		}

		// Set flag to use InflowWind or return error
		if len(files.InflowWind) > 0 {
			files.InflowWind[0].WindType.Value = 1
			files.InflowWind[0].HWindSpeed.Value = op.WindSpeed
			files.InflowWind[0].PLExp.Value = 0
		} else {
			return fmt.Errorf("no InflowWind files were imported")
		}

	} else {

		// Disable InflowWind and remove files
		files.Main[0].CompInflow.Value = 0
		files.InflowWind = []InflowWind{}

		// Disable AeroDyn and remove files
		files.Main[0].CompAero.Value = 0
		files.AeroDyn = []AeroDyn{}
		files.AeroDyn14 = []AeroDyn14{}

		// Disable ServoDyn and remove files
		files.Main[0].CompServo.Value = 0
		files.ServoDyn = []ServoDyn{}

		// Disable ElastoDyn generator DOF
		files.ElastoDyn[0].GenDOF.Value = false
	}

	// Write modified turbine files
	filePrefix := fmt.Sprintf("%02d_", op.ID)
	if err := files.Write(caseDir, filePrefix); err != nil {
		return fmt.Errorf("error writing turbine files: %w", err)
	}

	// Get number of linearization steps
	numLinSteps := files.Main[0].NLinTimes.Value

	// Create path to main file and log file
	mainName := filePrefix + files.Main[0].Name
	rootName := strings.TrimSuffix(mainName, filepath.Ext(mainName))
	rootPath := filepath.Join(caseDir, rootName)
	mainPath := filepath.Join(caseDir, mainName)
	logPath := rootPath + ".log"

	// Create log file
	logFile, err := os.Create(logPath)
	if err != nil {
		return fmt.Errorf("error creating log file '%s': %w", logFile.Name(), err)
	}
	defer logFile.Close()

	// Create status ID from operating point and set in linearization flag to false
	statusID := op.ID
	inLinearization := false

	//--------------------------------------------------------------------------
	// Run Linearization
	//--------------------------------------------------------------------------

	// Create command, get output pipe, set stderr to stdout, start command
	cmd := exec.CommandContext(ctx, p.Exec.Path, mainPath)
	outputReader, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	cmd.Start()

	// Get progress
	scanner := bufio.NewScanner(outputReader)
	scanner.Split(ScanLines)
	for scanner.Scan() {
		line := strings.Map(func(r rune) rune {
			if unicode.IsGraphic(r) {
				return r
			}
			return -1
		}, scanner.Text())
		line = strings.TrimSpace(line)
		if len(line) > 0 {
			runtime.EventsEmit(ctx, "evalLog", EvalLog{ID: statusID, Line: line})
		}
		logFile.WriteString(line + "\n")
		if strings.Contains(line, "Time: ") && !inLinearization {
			fields := strings.Fields(line)
			currentTime, err := strconv.ParseFloat(fields[1], 32)
			if err != nil {
				continue
			}
			totalTime, err := strconv.ParseFloat(fields[3], 32)
			if err != nil {
				continue
			}
			runtime.EventsEmit(ctx, "evalStatus", EvalStatus{
				ID:          statusID,
				State:       "Simulation",
				SimProgress: int(100 * currentTime / totalTime),
			})
		} else if strings.Contains(line, "Performing linearization") {
			inLinearization = true
			fields := strings.Fields(line)
			linNumber, err := strconv.ParseFloat(fields[2], 32)
			if err != nil {
				continue
			}
			runtime.EventsEmit(ctx, "evalStatus", EvalStatus{
				ID:          statusID,
				State:       "Linearization",
				SimProgress: 100,
				LinProgress: int(100 * linNumber / float64(numLinSteps)),
			})
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}

	// Wait for command to exit, set status on error
	if err := cmd.Wait(); err != nil {
		status := EvalStatus{
			ID:          statusID,
			State:       "Error",
			SimProgress: 100,
			LinProgress: 100,
			Error:       err.Error(),
		}
		// If context was canceled, set state to canceled
		if cause := context.Cause(ctx); cause != nil {
			status.State = "Canceled"
			status.Error = cause.Error()
		}
		runtime.EventsEmit(ctx, "evalStatus", status)
		return err
	}

	//--------------------------------------------------------------------------
	// Multi-Blade Coordinate Transform
	//--------------------------------------------------------------------------

	// Read linearization files for this operating point
	linFiles, err := filepath.Glob(filepath.Join(caseDir, filePrefix+"*.lin"))
	if err != nil {
		return err
	}

	// Read linearization data from files
	linFileData := make([]*mbc3.LinData, len(linFiles))
	for i, f := range linFiles {
		if linFileData[i], err = mbc3.ReadLinFile(f); err != nil {
			return err
		}
	}

	// Create matrix data from linearization file data
	matData := mbc3.NewMatData(linFileData)

	// Perform multi-blade coordinate transform
	mbc, err := matData.MBC3()
	if err != nil {
		return err
	}

	// Perform eigen analysis
	eigRes, err := mbc.EigenAnalysis()
	if err != nil {
		return err
	}

	// Write MBC data to file
	bs, err := json.MarshalIndent(mbc, "", "\t")
	if err != nil {
		return err
	}
	err = os.WriteFile(filepath.Join(caseDir, filePrefix+"mbc.json"), bs, 0777)
	if err != nil {
		return err
	}

	// Write Eigen analysis results data to file
	bs, err = json.MarshalIndent(eigRes.Modes, "", "\t")
	if err != nil {
		return err
	}
	err = os.WriteFile(filepath.Join(caseDir, filePrefix+"modes.json"), bs, 0777)
	if err != nil {
		return err
	}

	//--------------------------------------------------------------------------
	// Send complete status
	//--------------------------------------------------------------------------

	runtime.EventsEmit(ctx, "evalStatus", EvalStatus{
		ID:          statusID,
		State:       "Complete",
		SimProgress: 100,
		LinProgress: 100,
	})

	return nil
}

func ScanLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexAny(data, "\n\r"); i >= 0 {
		if data[i] == '\n' { // LF line
			return i + 1, data[0:i], nil
		} else if len(data) > i+1 && data[i+1] == '\n' { // CRLF line
			return i + 2, data[0:i], nil
		} else { // CR Line
			return i + 1, data[0:i], nil
		}
	}
	// If we're at EOF, we have a final, non-terminated line. Return it.
	if atEOF {
		return len(data), dropCR(data), nil
	}
	// Request more data.
	return 0, nil, nil
}

// dropCR drops a terminal \r from the data.
func dropCR(data []byte) []byte {
	if len(data) > 0 && data[len(data)-1] == '\r' {
		return data[0 : len(data)-1]
	}
	return data
}
