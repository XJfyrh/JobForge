# Vector retrieval

A vector index contains text chunks, source document identifiers, and embedding vectors. Queries are embedded by the same model and ranked with cosine similarity.

The persisted index identifies its corpus version, chunking algorithm, and model digest. A new corpus or model creates a different index version and requires a new business key.
