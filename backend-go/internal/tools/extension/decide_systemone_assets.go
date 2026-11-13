package extension

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"jad/backend-go/internal/tools"
)

// TypeSafe System One reference bounds (typesafe_images.md, "References"). They count UTF-8
// bytes, not characters, and are protocol bounds rather than settings.
const (
	decideSystemoneMaxReferenceBytes  = 1024
	decideSystemoneMaxReferencesBytes = 65536
	// decideSystemoneReferenceLabelBytes cuts a reference quoted in a message.
	decideSystemoneReferenceLabelBytes = 64
)

// decideSystemoneAsset is one image as the request carries it. The fields are written in
// this order, and DataBase64 is the caller's string, never decoded or altered.
type decideSystemoneAsset struct {
	Type       string `json:"type"`
	Format     string `json:"format"`
	DataBase64 string `json:"data_base64"`
}

// normalizeDecideSystemoneAssets accepts any letter case and padding in a format, copying
// only the maps that need it. It returns nil for a null or empty assets, which stand for
// none. The data_base64 string is never copied, trimmed or changed.
func normalizeDecideSystemoneAssets(value any) any {
	assets, ok := value.(map[string]any)
	if !ok {
		return value
	}
	if len(assets) == 0 {
		return nil
	}
	var fixed map[string]any
	for reference, item := range assets {
		asset, ok := item.(map[string]any)
		if !ok {
			continue
		}
		format, ok := asset["format"].(string)
		if !ok {
			continue
		}
		lower := strings.ToLower(strings.TrimSpace(format))
		if lower == format {
			continue
		}
		if fixed == nil {
			fixed = make(map[string]any, len(assets))
			for key, item := range assets {
				fixed[key] = item
			}
		}
		copied := make(map[string]any, len(asset))
		for key, field := range asset {
			copied[key] = field
		}
		copied["format"] = lower
		fixed[reference] = copied
	}
	if fixed == nil {
		return assets
	}
	return fixed
}

// decideSystemoneReferenceLabel quotes a reference for a message, cut so a message stays
// short. Only references are quoted; a message never carries image data.
func decideSystemoneReferenceLabel(reference string) string {
	cut, was := tools.CutText(reference, decideSystemoneReferenceLabelBytes)
	if was {
		return fmt.Sprintf("%q...", cut)
	}
	return fmt.Sprintf("%q", cut)
}

// parseDecideSystemoneAssets turns the assets argument into images and refuses what the
// provider would refuse by shape: a reference that is blank or too long, references that
// together are too long, and anything but {type?: image, format, data_base64}. Nothing here
// reads the image data; checkDecideSystemoneAssetData does, after the size check. The
// result is nil when there are no assets. References are visited in sorted order, so the
// refusal for a given call is always the same.
func parseDecideSystemoneAssets(raw any) (map[string]decideSystemoneAsset, error) {
	if raw == nil {
		return nil, nil
	}
	items, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("assets must be an object keyed by reference")
	}
	if len(items) == 0 {
		return nil, nil
	}
	references := make([]string, 0, len(items))
	for reference := range items {
		references = append(references, reference)
	}
	slices.Sort(references)
	assets := make(map[string]decideSystemoneAsset, len(items))
	total := 0
	for _, reference := range references {
		label := decideSystemoneReferenceLabel(reference)
		switch {
		case strings.TrimSpace(reference) == "":
			return nil, errors.New("an asset reference is empty or only whitespace")
		case len(reference) > decideSystemoneMaxReferenceBytes:
			return nil, fmt.Errorf("asset %s: the reference is %d bytes; the limit is %d", label, len(reference), decideSystemoneMaxReferenceBytes)
		}
		if total += len(reference); total > decideSystemoneMaxReferencesBytes {
			return nil, fmt.Errorf("the asset references together are over %d bytes; use shorter ones", decideSystemoneMaxReferencesBytes)
		}
		fields, ok := items[reference].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("asset %s must be an object with format and data_base64", label)
		}
		keys := make([]string, 0, len(fields))
		for key := range fields {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			switch key {
			case "type", "format", "data_base64":
			default:
				cut, _ := tools.CutText(key, decideSystemoneReferenceLabelBytes)
				return nil, fmt.Errorf("asset %s: %q is not accepted; send the image as format and data_base64", label, cut)
			}
		}
		if kind, present := fields["type"]; present && kind != "image" {
			return nil, fmt.Errorf("asset %s: type must be image", label)
		}
		format, _ := fields["format"].(string)
		if format != "png" && format != "jpg" && format != "jpeg" {
			return nil, fmt.Errorf("asset %s: format must be png, jpg or jpeg", label)
		}
		data, ok := fields["data_base64"].(string)
		if !ok {
			return nil, fmt.Errorf("asset %s: data_base64 is missing or not a string", label)
		}
		assets[reference] = decideSystemoneAsset{Type: "image", Format: format, DataBase64: data}
	}
	return assets, nil
}

// decideSystemoneAssetsSize is the byte size of the assets object as the request carries
// it, worked out without encoding the data: the keys are encoded one by one (they are
// small) and the data is its length, since base64 needs no escaping. It is exact for data
// that passed checkDecideSystemoneAssetData, which runs after the size check.
func decideSystemoneAssetsSize(assets map[string]decideSystemoneAsset) int {
	if len(assets) == 0 {
		return 0
	}
	const (
		head = len(`:{"type":"image","format":"`)
		mid  = len(`","data_base64":"`)
		tail = len(`"}`)
	)
	size := 2 + len(assets) - 1 // the braces and the commas
	for reference, asset := range assets {
		key, err := tools.MarshalJSON(reference)
		if err != nil {
			key = nil
		}
		size += len(key) + head + len(asset.Format) + mid + len(asset.DataBase64) + tail
	}
	return size
}

// checkDecideSystemoneAssetData refuses data that is not standard padded base64, which
// the provider also refuses (400): the standard alphabet with = padding, a length that is a
// multiple of four, and no whitespace, line breaks or data: prefix. It scans the string
// once and allocates nothing; the image itself is checked by the provider.
func checkDecideSystemoneAssetData(assets map[string]decideSystemoneAsset) error {
	if len(assets) == 0 {
		return nil
	}
	references := make([]string, 0, len(assets))
	for reference := range assets {
		references = append(references, reference)
	}
	slices.Sort(references)
	for _, reference := range references {
		if !decideSystemoneStandardBase64(assets[reference].DataBase64) {
			return fmt.Errorf("asset %s: data_base64 must be standard base64 with = padding, no whitespace and no data: prefix", decideSystemoneReferenceLabel(reference))
		}
	}
	return nil
}

func decideSystemoneStandardBase64(data string) bool {
	length := len(data)
	if length == 0 || length%4 != 0 {
		return false
	}
	body := length
	for body > length-2 && data[body-1] == '=' {
		body--
	}
	for index := 0; index < body; index++ {
		switch character := data[index]; {
		case character >= 'A' && character <= 'Z', character >= 'a' && character <= 'z', character >= '0' && character <= '9', character == '+', character == '/':
		default:
			return false
		}
	}
	return true
}
