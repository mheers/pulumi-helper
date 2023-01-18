# Pulumi-Helper (ph)

Pulumi-Helper (ph) is a command line tool to help you manage your Pulumi workspaces and stacks.

## Installation

### Install from source

```bash
git clone
cd pulumi-helper
go install
```

### Install from docker

```bash
docker pull mheers/pulumi-helper:latest
```

### Install from go

```bash
go install github.com/mheers/pulumi-helper@latest
```

## Usage

```bash
ph help
```

## Use cases

- [x] List all stacks in a workspace
- [x] List all workspaces with their current stack and the last update time
- [x] Select a stack in a workspace

### Write the current stack in your shell prompt

```bash
alias ph='pulumi-helper'
export PROMPT='$(ph st n -i)'$PROMPT
```
