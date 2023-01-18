package workspace

import "testing"

func TestGetWorkspaceNameAndHashFromFile(t *testing.T) {
	tests := []struct {
		name    string
		file    string
		want    string
		want1   string
		wantErr bool
	}{
		{
			name:    "test1",
			file:    "pulumi-demo-049bc369530d2f05a8ba2cdbbb49164cfd3ba066-workspace.json",
			want:    "pulumi-demo",
			want1:   "049bc369530d2f05a8ba2cdbbb49164cfd3ba066",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, got1, err := getWorkspaceNameAndHashFromFile(tt.file)
			if (err != nil) != tt.wantErr {
				t.Errorf("getWorkspaceNameAndHashFromFile() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("getWorkspaceNameAndHashFromFile() got = %v, want %v", got, tt.want)
			}
			if got1 != tt.want1 {
				t.Errorf("getWorkspaceNameAndHashFromFile() got1 = %v, want %v", got1, tt.want1)
			}
		})
	}

}
