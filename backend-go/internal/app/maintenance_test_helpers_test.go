package app

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func postEmptyMutation(t *testing.T, address, token string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, address, strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func readMutationResponse(t *testing.T, response *http.Response) string {
	t.Helper()
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
