# ADR-004: Local BlobStore

Domain code addresses blobs by validated logical key through `Put`, `Get`, `Exists`, `Stat`, and `Delete`. V1 uses a filesystem adapter rooted at `data/blobstore`.

