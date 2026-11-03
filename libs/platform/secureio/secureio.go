// Package secureio lê arquivos de configuração/segredos com validação explícita.
package secureio

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// MaxConfigSize limita o tamanho de qualquer arquivo de configuração lido.
const MaxConfigSize = 1 << 20

// ReadFile lê um arquivo regular de configuração. Exige caminho absoluto sem
// segmentos ".." e arquivo regular (não diretório/dispositivo/FIFO), e limita o tamanho.
// O acesso é ancorado no diretório via os.Root, de modo que links simbólicos não
// escapam dele (os links internos usados por Kubernetes/Docker secrets continuam válidos).
func ReadFile(path string) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("path must be absolute: %q", path)
	}
	if filepath.Clean(path) != path {
		return nil, fmt.Errorf("path must be clean (no '..', '.' or duplicate separators): %q", path)
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(filepath.Base(path))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%q is not a regular file", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxConfigSize+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxConfigSize {
		return nil, errors.New("file exceeds size limit")
	}
	return b, nil
}
