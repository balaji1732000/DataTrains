# Local blob storage

The runtime root defaults to `data/blobstore` and is ignored by Git. Business logic uses logical keys such as `raw/sessions/sess_001/video/000001.mp4`; it never depends on physical paths.

