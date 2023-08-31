<img src="https://luctus.at/logos/miyuki.png" width="64" />

# miyuki

Miyuki is an automatic SFTP downloader that puts files in a .tar.gz file.

It was made with the intention of automating the backups of systems that do not grant you ssh but only sftp access.  
(This solution was made for systems were it is not possible to push data but only pull data, e.g. rented gameservers which only give you ftp access)


# Features

 - Automatic download of multiple files and folders via SFTP with cronjob definition
 - Filter wanted files via white- and black-list for filetypes or full names
 - Stream files directly from the remote target into the gzip file with no intermediate storage required
 - (Optional) Automatically deletes backups older than configured hours
 - JSON logging ready for elastic or other log systems
 - Small status page for viewing storage usage and job runtimes

# Install

Simply clone the repo, copy&edit the config, get the modules, build it and run the server:

```bash
git clone https://github.com/OverlordAkise/miyuki.git
cd miyuki
cp config.example.yaml config.yaml
# edit the config with e.g. vim
go get .
go build .
./miyuki
```

The backup folders will be automatically created according to your config.


# TODO

 - Add prometheus monitoring support

# Config

 - The `Retryfirstfailed` config parameter enables jobs who fail to be tried again after a 5s delay. This is because some SFTP servers I have encountered reset the first connection they get, so this helps with accessing those systems. (I don't know why this happens on their server, this is a workaround for my problem)
 - The black-/whitelist can consist of either a full name (`myfile.txt`) or a file ending wildcard (`*.txt`)
 - The blacklist blocks any folder or file from being downloaded. The whitelist only allows the files in the whitelist to be downloaded. The whitelist does NOT block folders from being downloaded, it only blocks files if they are not on the list.

# About FTP

The secsy/goftp library has been used instead of jlaffaye/ftp because (for whatever reason) the jlaffaye one disconnects after downloading 5 files with a 226 code. (A "success" code)

The FTP download has been tested with a Port 21 listening FTP server that uses Explicit TLS and EPSV.

From online sources it is highly recommended to use SFTP instead of FTP for transfering data.

# Credits

The logo of miyuki has been drawn by the amazing 'HollyMoon' (Discord: hollymoon)
