-- +goose Up
-- Team writes for different teams no longer serialize on one lock (F21-20, audit S2). A write for one
-- team holds team_write_fence in ROW SHARE mode (it conflicts only with the global EXCLUSIVE lock) and
-- its bucket, team_write_fence_NN, in EXCLUSIVE mode; a write that spans teams still takes the global
-- lock. All are LOCK TABLE, which takes no snapshot (see 158). Prior art: NetBird serializes writes per
-- account (management/server/store: account lock), not across accounts.
CREATE TABLE paperboat.team_write_fence_00 ();
CREATE TABLE paperboat.team_write_fence_01 ();
CREATE TABLE paperboat.team_write_fence_02 ();
CREATE TABLE paperboat.team_write_fence_03 ();
CREATE TABLE paperboat.team_write_fence_04 ();
CREATE TABLE paperboat.team_write_fence_05 ();
CREATE TABLE paperboat.team_write_fence_06 ();
CREATE TABLE paperboat.team_write_fence_07 ();
CREATE TABLE paperboat.team_write_fence_08 ();
CREATE TABLE paperboat.team_write_fence_09 ();
CREATE TABLE paperboat.team_write_fence_10 ();
CREATE TABLE paperboat.team_write_fence_11 ();
CREATE TABLE paperboat.team_write_fence_12 ();
CREATE TABLE paperboat.team_write_fence_13 ();
CREATE TABLE paperboat.team_write_fence_14 ();
CREATE TABLE paperboat.team_write_fence_15 ();

-- +goose Down
DROP TABLE paperboat.team_write_fence_00;
DROP TABLE paperboat.team_write_fence_01;
DROP TABLE paperboat.team_write_fence_02;
DROP TABLE paperboat.team_write_fence_03;
DROP TABLE paperboat.team_write_fence_04;
DROP TABLE paperboat.team_write_fence_05;
DROP TABLE paperboat.team_write_fence_06;
DROP TABLE paperboat.team_write_fence_07;
DROP TABLE paperboat.team_write_fence_08;
DROP TABLE paperboat.team_write_fence_09;
DROP TABLE paperboat.team_write_fence_10;
DROP TABLE paperboat.team_write_fence_11;
DROP TABLE paperboat.team_write_fence_12;
DROP TABLE paperboat.team_write_fence_13;
DROP TABLE paperboat.team_write_fence_14;
DROP TABLE paperboat.team_write_fence_15;
