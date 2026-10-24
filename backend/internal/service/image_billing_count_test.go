package service

import "testing"

func TestResolveBillableImageCount(t *testing.T) {
	tests := []struct {
		name     string
		reported int
		want     int
	}{
		{name: "实际返回多张", reported: 3, want: 3},
		{name: "实际缺失按一张兜底", reported: 0, want: 1},
		{name: "非法负数按一张兜底", reported: -1, want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveBillableImageCount(tt.reported); got != tt.want {
				t.Fatalf("resolveBillableImageCount(%d) = %d, want %d", tt.reported, got, tt.want)
			}
		})
	}
}

func TestCountOpenAIImageOutputs_OfficialMultiImageResponse(t *testing.T) {
	body := []byte(`{
		"created": 1792815229,
		"data": [
			{"generation_id": "b235a66f-decc-444a-9689-4be8a322a6d0", "url": "https://images.example/a.png"},
			{"generation_id": "de2ccab0-ab61-46bb-9e7b-879af4837dee", "url": "https://images.example/b.png"},
			{"generation_id": "18817363-3449-4217-ba6f-9019ef7fd760", "url": "https://images.example/c.png"}
		],
		"usage": {"input_tokens": 54, "output_tokens": 586, "total_tokens": 640},
		"size": "1024x1024",
		"quality": "low"
	}`)

	if got := countOpenAIResponseImageOutputsFromJSONBytes(body); got != 3 {
		t.Fatalf("countOpenAIResponseImageOutputsFromJSONBytes() = %d, want 3", got)
	}
	if got := resolveBillableImageCount(countOpenAIResponseImageOutputsFromJSONBytes(body)); got != 3 {
		t.Fatalf("resolveBillableImageCount() = %d, want 3", got)
	}
}
