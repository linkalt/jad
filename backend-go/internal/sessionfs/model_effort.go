package sessionfs

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// effortSeparator splits "<category>/<name>@<effort>" into the model ID and the reasoning
// effort chosen for one run or agent.
const effortSeparator = "@"

// SplitModelEffort splits a model reference into the model ID and the effort ("" when the
// reference has none). An empty or malformed effort is refused, so "jad/sonet@" is never
// read as the plain model.
func SplitModelEffort(reference string) (string, string, error) {
	id, effort, found := strings.Cut(reference, effortSeparator)
	if !found {
		return reference, "", nil
	}
	if id == "" {
		return "", "", &ClientError{Message: fmt.Sprintf("model %q: the model before %q must not be empty", reference, effortSeparator)}
	}
	if err := checkEffortName(effort); err != nil {
		return "", "", &ClientError{Message: fmt.Sprintf("model %q: %v", reference, err)}
	}
	return id, effort, nil
}

func checkEffortName(effort string) error {
	if effort == "" {
		return fmt.Errorf("the reasoning effort after %q must not be empty", effortSeparator)
	}
	for _, r := range effort {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("the reasoning effort %q may only hold letters, digits, '-', '_' and '.'", effort)
		}
	}
	return nil
}

// effortsField reads efforts: the reasoning efforts a run may choose with "@<effort>".
func effortsField(fields map[string]json.RawMessage, path string) ([]string, error) {
	raw, ok := fields["efforts"]
	if !ok || strings.TrimSpace(string(raw)) == "null" {
		return nil, nil
	}
	var efforts []string
	if err := json.Unmarshal(raw, &efforts); err != nil {
		return nil, configError("%s: efforts must be an array of strings", path)
	}
	for index, effort := range efforts {
		if err := checkEffortName(effort); err != nil {
			return nil, configError("%s: efforts[%d]: %v", path, index, err)
		}
		if slices.Index(efforts, effort) != index {
			return nil, configError("%s: efforts[%d]: %q is listed twice", path, index, effort)
		}
	}
	return efforts, nil
}

// CheckModelEffort reads the model file and refuses an effort it does not list in efforts.
// An empty effort is not checked.
func CheckModelEffort(modelPath, effort string) error { return checkModelEffort(modelPath, effort) }

func checkModelEffort(modelPath, effort string) error {
	if effort == "" {
		return nil
	}
	model, err := loadModel(modelPath, "", "")
	if err != nil {
		return err
	}
	_, err = model.withEffort(effort)
	return err
}

// withEffort returns the model with request.reasoning.effort set to the chosen effort. The
// model file must list the effort in efforts.
func (m Model) withEffort(effort string) (Model, error) {
	if effort == "" {
		return m, nil
	}
	if len(m.Efforts) == 0 {
		return Model{}, &ClientError{Message: fmt.Sprintf("model %s does not offer a reasoning effort choice", m.Name)}
	}
	if !slices.Contains(m.Efforts, effort) {
		return Model{}, &ClientError{Message: fmt.Sprintf("model %s does not offer the reasoning effort %q (available: %s)", m.Name, effort, strings.Join(m.Efforts, ", "))}
	}
	request := make(map[string]json.RawMessage)
	if err := json.Unmarshal(m.Request, &request); err != nil || request == nil {
		return Model{}, configError("model %s: request must be a JSON object", m.Name)
	}
	reasoning := make(map[string]json.RawMessage)
	if raw, ok := request["reasoning"]; ok && strings.TrimSpace(string(raw)) != "null" {
		if err := json.Unmarshal(raw, &reasoning); err != nil || reasoning == nil {
			return Model{}, configError("model %s: request.reasoning must be a JSON object to take a reasoning effort", m.Name)
		}
	}
	encodedEffort, err := json.Marshal(effort)
	if err != nil {
		return Model{}, err
	}
	reasoning["effort"] = encodedEffort
	if request["reasoning"], err = json.Marshal(reasoning); err != nil {
		return Model{}, err
	}
	if m.Request, err = json.Marshal(request); err != nil {
		return Model{}, err
	}
	m.Effort = effort
	return m, nil
}

// checkModelEffortOnly is checkModelEffort for a model file that may fail the model rules:
// it reports only an effort the model does not offer, and none when the file cannot be
// loaded, which is reported where the model itself is judged.
func checkModelEffortOnly(modelPath, effort string) error {
	if effort == "" {
		return nil
	}
	model, err := loadModel(modelPath, "", "")
	if err != nil {
		return nil
	}
	_, err = model.withEffort(effort)
	return err
}
