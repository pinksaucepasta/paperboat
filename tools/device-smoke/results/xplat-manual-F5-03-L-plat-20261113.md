# F5-03 on Linux, 2026-11-13 (IST), by hand

Machines: client plat-c1 and target plat-s3, both hp systemd containers (Ubuntu 24.04, one account, managed SSH), pb dev-3ef4b7b39 (main) on both (`pb version`). Commands: `pb scp -r`, `pb sftp -b` (put/get -a), `pb rsync -a --partial --append-verify`. Tree: 602 files, 407.5 MiB (600 small files 2-32 KiB + two 200 MiB files). Interruption: scp/sftp client killed at 4 s (SIGKILL), rsync interrupted with SIGINT to its process group (Ctrl-C), then the same command rerun (sftp with -a, rsync with --partial --append-verify). Pass = sha256 of every file equal on both sides after the rerun and the partial was smaller than the file at the interruption. Script: a small python driver run on the client (not committed; a harness, not a product part).

## sftp (run 2, 13 Nov 02:00)
PASS sftp up large-tree ok 602 files 14s
PASS sftp down large-tree ok 602 files 16s
PASS sftp up resume interrupted=True partial=56401920 of 209715200 bytes; resumed ok 5s; hash match=True
PASS sftp down resume interrupted=True partial=60579840 of 209715200 bytes; resumed ok 5s; hash match=True

## scp and rsync large tree (run 3)
PASS scp up large-tree ok 602 files 14s
PASS scp down large-tree ok 602 files 190s
PASS scp up resume interrupted=True partial=10183680 of 209715200 bytes; resumed ok 52s; hash match=True
PASS scp down resume interrupted=True partial=125076480 of 209715200 bytes; resumed ok 6s; hash match=True
PASS rsync up large-tree ok 602 files 13s
PASS rsync down large-tree ok 602 files 12s

## scp, first attempt (run 2): FAIL once, rerun above passed
FAIL scp up large-tree rc1 client_loop: send disconnect: Broken pipe

## rsync resume, both directions, by hand (rs_hand.sh, 200 MiB random file)
src sha256 50e4a361b42cf58b7798ebbc86ee25ff4863f5def8c7f8eddaccae54c97a86a7
== rsync up: interrupt (SIGINT at 4 s) ==
client_loop: send disconnect: Broken pipe
rsync error: unexplained error (code 255) at rsync.c(716) [sender=3.2.7]
first run exit=124
remote partial 137494528 bytes
.
..
up.bin
== rsync up: resume ==
resume exit=0
resume took 2 s
remote sha256 50e4a361b42cf58b7798ebbc86ee25ff4863f5def8c7f8eddaccae54c97a86a7
== rsync down: interrupt ==
rsync error: unexplained error (code 255) at rsync.c(716) [generator=3.2.7]
rsync: [generator] write error: Broken pipe (32)
rsync error: received SIGINT, SIGTERM, or SIGHUP (code 20) at io.c(519) [receiver=3.2.7]
first run exit=124
local partial 138149888 bytes
== rsync down: resume ==
resume exit=0
resume took 3 s
local sha256 50e4a361b42cf58b7798ebbc86ee25ff4863f5def8c7f8eddaccae54c97a86a7
rc=0
