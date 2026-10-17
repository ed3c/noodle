/** Navigation groups native linked worktrees; names are never identity. */
export function repositoryGroups(repositories) {
  const groups = new Map();
  for (const repo of repositories) {
    const o = repo.observation;
    const key = o?.status === "observed" ? o.repository_id : `legacy:${repo.root || repo.id}`;
    if (!groups.has(key)) {
      groups.set(key, { id: key, label: repo.label, members: [] });
    }
    groups.get(key).members.push(repo);
  }
  return [...groups.values()];
}
export function branchName(repo) {
  const o = repo.observation;
  if (o?.status !== "observed") {
    return "UNKNOWN";
  }
  return o.detached ? "DETACHED" : o.branch;
}
export function worktreeKey(repo) {
  return repo.observation?.worktree_id || `legacy:${repo.root || repo.id}`;
}
export function filterRepositories(repositories, selection) {
  const groups = repositoryGroups(repositories);
  const members = selection.repo
    ? groups.find((g) => g.id === selection.repo)?.members || []
    : repositories;
  return members.filter(
    (r) =>
      (!selection.branch || branchName(r) === selection.branch) &&
      (!selection.worktree || worktreeKey(r) === selection.worktree) &&
      (!selection.snapshot || r.id === selection.snapshot),
  );
}

export function dirtyName(value) {
  if (value === true) {
    return "有未提交變更";
  }
  if (value === false) {
    return "乾淨";
  }
  return "狀態 UNKNOWN";
}
export function groupView(group) {
  return { ...group.members[0], id: group.id, label: group.label, members: group.members };
}
