# miyuki

Miyuki is an automatic SFTP downloader that puts files in a .tar.gz file.

It was made with the intention of automating the backups of systems that do not grant you ssh but only sftp access.

**Warning**: This has not been thoroughly tested. This message will be removed after testings have finished.

# Features

 - Automatic download of multiple files and folders via SFTP with cronjob definition
 - Filter wanted files via white- and black-list for filetypes or full names
 - Stream files directly from the remote target into the gzip file with no intermediate storage required
 - (Optional) Automatically deletes backups older than configured hours
 - JSON logging ready for elastic or other log systems
 - Small status page for viewing storage usage and job runtimes


# TODO

 - Add prometheus monitoring support
 - Add FTP support (barely anyone uses it anymore)
