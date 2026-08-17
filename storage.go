package lebro

import "github.com/tesh254/lebro/internal/runtime"

func NewMemoryStore() *MemoryStore                    { return runtime.NewMemoryStore() }
func NewSQLiteStore(dsn string) (*SQLiteStore, error) { return runtime.NewSQLiteStore(dsn) }

func NewPostgresStore(dsn string, opts PostgresStoreOptions) (*PostgresStore, error) {
	return runtime.NewPostgresStore(dsn, opts)
}

func NewMemoryVectorStore() *MemoryVectorStore { return runtime.NewMemoryVectorStore() }

func NewSQLiteVectorStore(dsn string) (*SQLiteVectorStore, error) {
	return runtime.NewSQLiteVectorStore(dsn)
}

func NewPostgresVectorStore(dsn string, opts PostgresVectorStoreOptions) (*PostgresVectorStore, error) {
	return runtime.NewPostgresVectorStore(dsn, opts)
}

func NewQdrantVectorStore(config QdrantVectorStoreConfig) (*QdrantVectorStore, error) {
	return runtime.NewQdrantVectorStore(config)
}
