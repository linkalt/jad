# Task: Design Parallel Agent Workspace Management

Design a workspace isolation and integration system for the existing Go agent harness using **Bubblewrap + OverlayFS + Git worktrees**.

## Rules

- Do not limit tools execution, but limit the environment

## Objective

Allow multiple agents, subagents, profiles, and tools to execute simultaneously with independent write access without duplicating the entire project workspace.

Each agent must:
- See the same initial project state.
- Have an independent writable environment.
- Be unable to modify another agent's workspace.
- Preserve its changes after execution.
- Transfer only its changes for integration.
- Support concurrent execution without corrupting the main workspace.

The main goal is minimizing disk usage, workspace initialization time, and integration overhead.

## 1. Mandatory Codebase Exploration

Inspect the existing implementation before proposing changes.

Trace relevant components:
- Agent and subagent lifecycle.
- Profile execution and tool execution.
- Workspace and working-directory management.
- Process execution and command handling.
- Bubblewrap or existing sandbox implementation, if present.
- Git operations and repository management.
- Task dependencies and concurrent job execution.
- Cleanup, cancellation, and error recovery.
- Configuration and persistence.

Identify existing reusable components. Avoid introducing duplicate abstractions.

Do not modify code during this task.

## 2. Proposed Architecture

Design the integration of three technologies:

**Bubblewrap**
- Creates isolated execution environments.
- Restricts filesystem access.
- Controls writable directories, mounts, devices, and namespaces.
- Prevents agents from accessing other agents' writable environments.

**OverlayFS**
- Shared read-only base workspace.
- Independent upperdir and workdir for each agent.
- Individual merged workspace for each agent.
- Persistent changes after agent termination.
- Efficient initialization without copying the entire workspace.

**Git worktrees**
- Independent Git working directories and branches.
- Shared Git object database.
- Agent-specific branches.
- Change tracking and integration.
- Conflict detection and resolution.

Evaluate how OverlayFS and Git worktrees should interact. In particular, determine whether worktrees should be created inside each agent's overlay or whether another arrangement is safer and more efficient.

Do not assume that all three technologies can be combined without filesystem and Git compatibility issues. Identify and resolve those issues in the design.

## 3. Workspace Lifecycle

Design the complete lifecycle:

1. Create agent workspace.
2. Initialize isolated filesystem.
3. Attach the agent's Git worktree or branch.
4. Start Bubblewrap with appropriate filesystem restrictions.
5. Execute agent tools and commands.
6. Track modified, added, deleted, and renamed files.
7. Preserve agent changes after execution.
8. Collect changes.
9. Validate changes against the integration target.
10. Merge accepted changes into the main workspace.
11. Handle conflicts.
12. Cleanup workspace after successful integration or cancellation.

Support both successful and failed executions.

## 4. Concurrency and Integration

Design for multiple agents writing simultaneously.

Consider:
- Agents modifying different files.
- Agents modifying the same file.
- Agents creating files in the same directory.
- Agent dependencies.
- Agents based on different versions of the project.
- One agent finishing while another is still running.
- Cancellation during file writes.
- Agent crash and recovery.
- Main workspace changing while agents are running.

Define a safe integration strategy.

Never automatically overwrite another agent's changes.

## 5. Security Requirements

Treat agents as untrusted processes.

Evaluate:
- Filesystem isolation.
- Read-only base workspace.
- Access to other agent workspaces.
- Symlink and mount escape risks.
- Access to Git metadata and credentials.
- Network access.
- Process and namespace isolation.
- Temporary files and cleanup.

Clearly distinguish filesystem isolation from process, network, and privilege isolation.

## 6. Storage and Performance

Compare the following:
- Full workspace copies.
- Git worktrees alone.
- OverlayFS alone.
- OverlayFS combined with Git worktrees.
- Btrfs snapshots as an alternative.

Estimate:
- Workspace creation cost.
- Disk consumption.
- Modified-file copy-up cost.
- Change collection cost.
- Integration overhead.

Prioritize practical efficiency over theoretical optimization.

## 7. Go Implementation Design

Propose the necessary components and responsibilities.

Consider interfaces similar to:

- WorkspaceManager
- Workspace
- SandboxManager
- OverlayManager
- WorktreeManager
- ChangeCollector
- WorkspaceIntegrator

These are conceptual examples, not mandatory names. Reuse existing project abstractions whenever possible.

Define:
- Data structures.
- Lifecycle methods.
- Resource ownership.
- Error handling.
- Cleanup guarantees.
- Persistent state.
- Configuration.
- Integration with the existing agent execution pipeline.

Avoid unnecessary interfaces and abstraction layers.

## 8. Deliverables

Produce an implementation-ready design document containing:

1. Existing architecture findings with actual file and symbol references.
2. Proposed architecture and component relationships.
3. Workspace lifecycle.
4. Filesystem layout.
5. Git integration strategy.
6. Concurrency and conflict handling.
7. Security model.
8. Storage and performance considerations.
9. Go interfaces and data structures.
10. Failure recovery and cleanup.
11. Testing strategy.
12. Implementation phases with dependencies.
13. Risks, limitations, and unresolved decisions.

Include architecture diagrams where useful.

## 9. Design Constraints

- Existing Go architecture must remain the foundation.
- Avoid unnecessary refactoring.
- Minimize tool calls and filesystem operations.
- Do not copy the entire workspace for every agent.
- Prefer backend defaults over unnecessary configuration.
- Avoid speculative abstractions.
- Preserve existing execution behavior where possible.
- Trace callers and dependencies before recommending modifications.
- Identify unrelated existing issues if discovered, but keep them separate from this design.

## Final Requirement

Do not implement anything.

Return a concrete technical design that another execution agent can follow without having to rediscover the architecture or make major architectural decisions.

Explicitly identify the recommended architecture, explain why it is preferable, and list any decisions that require approval before implementation.
