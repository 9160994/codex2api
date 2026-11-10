import { memo, useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";
import {
  FlaskConical,
  Loader2,
  Pencil,
  Plus,
  RefreshCw,
  Search,
  ServerCog,
  Trash2,
  Upload,
} from "lucide-react";
import { api } from "../api";
import type { ProxyRow } from "../api";
import type { WorkBuddyAccount } from "../types";
import ChannelLogo from "../components/ChannelLogo";
import { CompactStat } from "../components/CompactStat";
import PageHeader from "../components/PageHeader";
import Pagination from "../components/Pagination";
import StateShell from "../components/StateShell";
import StatusBadge from "../components/StatusBadge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { SegmentedPillGroup } from "@/components/ui/segmented-pill-group";
import { Switch } from "@/components/ui/switch";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { useConfirmDialog } from "../hooks/useConfirmDialog";
import { DEFAULT_PAGE_SIZE_OPTIONS, usePersistedPageSize } from "../hooks/usePersistedPageSize";
import { useToast } from "../hooks/useToast";
import { cn } from "@/lib/utils";
import { getErrorMessage } from "../utils/error";
import { formatBeijingTime } from "../utils/time";
import {
  workBuddyAccountLabel,
  workBuddyExpiryState,
  type WorkBuddyExpiryState,
} from "../lib/workbuddyImport";
import WorkBuddyAddModal from "./workbuddy/WorkBuddyAddModal";
import WorkBuddyEditModal from "./workbuddy/WorkBuddyEditModal";
import WorkBuddyImportModal from "./workbuddy/WorkBuddyImportModal";

type StatusFilter = "all" | "enabled" | "disabled" | "expired";
type RowAction = "test" | "refresh" | "delete" | "toggle";

const TOGGLE_ON = "data-[state=checked]:bg-primary";

function realmLabel(realm: string, t: (key: string) => string): string {
  const normalized = realm.trim().toLowerCase();
  if (!normalized) return t("workbuddy.realmUnknown");
  if (normalized === "global") return t("workbuddy.realmGlobal");
  if (normalized === "cn" || normalized === "china") return t("workbuddy.realmCN");
  return realm;
}

function expiryTone(state: WorkBuddyExpiryState): string {
  if (state === "expired") return "text-destructive";
  if (state === "soon") return "text-amber-600 dark:text-amber-300";
  return "text-muted-foreground";
}

const EXPIRY_CLASS = "whitespace-nowrap text-[12px]";

/**
 * WorkBuddy(wb2api) 账号页：与 Codex/Grok/Antigravity/Claude 账号页并列的第五个上游视图。
 *
 * 后端契约：
 *   GET    /accounts/workbuddy                  列表
 *   POST   /accounts/workbuddy                  新增
 *   POST   /accounts/workbuddy/import           批量导入（items 或 text）
 *   POST   /accounts/workbuddy/delete           删除（ids）
 *   POST   /accounts/workbuddy/models           同步上游模型目录
 *   PATCH  /accounts/{id}/workbuddy             名称 / 代理 / 凭据 / 过期时间 / 模型白名单
 *   POST   /accounts/{id}/workbuddy/test        测试连接
 *   POST   /accounts/{id}/workbuddy/refresh     刷新 token
 * 启停复用通用接口 POST /accounts/{id}/enable；模型白名单必须走 workbuddy PATCH
 * （通用 PATCH /accounts/{id}/models 对中转账号返回 400）。
 */
function WorkBuddyAccounts({ headerSlot }: { headerSlot?: ReactNode } = {}) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const { confirm, confirmDialog } = useConfirmDialog();
  const [pageSize, setPageSize] = usePersistedPageSize(
    "workbuddy-accounts",
    20,
    DEFAULT_PAGE_SIZE_OPTIONS,
  );

  const [accounts, setAccounts] = useState<WorkBuddyAccount[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [page, setPage] = useState(1);
  const [searchQuery, setSearchQuery] = useState("");
  const [statusFilter, setStatusFilter] = useState<StatusFilter>("all");
  const [selected, setSelected] = useState<Set<number>>(() => new Set());
  const [busy, setBusy] = useState<{ id: number; action: RowAction } | null>(null);
  const [batchBusy, setBatchBusy] = useState(false);

  const [proxies, setProxies] = useState<ProxyRow[]>([]);
  const [showAdd, setShowAdd] = useState(false);
  const [showImport, setShowImport] = useState(false);
  const [editAccount, setEditAccount] = useState<WorkBuddyAccount | null>(null);

  const requestRef = useRef<AbortController | null>(null);

  const reload = useCallback(async (options?: { silent?: boolean }) => {
    requestRef.current?.abort();
    const controller = new AbortController();
    requestRef.current = controller;
    if (!options?.silent) setLoading(true);
    try {
      const response = await api.listWorkBuddyAccounts(controller.signal);
      if (controller.signal.aborted) return;
      const rows = response.accounts ?? [];
      setAccounts(rows);
      setError(null);
      // 列表刷新后丢掉已不存在的选中项，避免批量删除时携带脏 id。
      const alive = new Set(rows.map((account) => account.id));
      setSelected((current) => {
        const next = new Set(Array.from(current).filter((id) => alive.has(id)));
        return next.size === current.size ? current : next;
      });
    } catch (loadError) {
      if (controller.signal.aborted) return;
      setError(getErrorMessage(loadError));
    } finally {
      if (!controller.signal.aborted) setLoading(false);
    }
  }, []);

  useEffect(() => {
    void reload();
    return () => requestRef.current?.abort();
  }, [reload]);

  // 代理池只在打开表单时才用到：拉取失败静默留空，手填代理仍然可用。
  useEffect(() => {
    let cancelled = false;
    void api
      .listProxies()
      .then((res) => {
        if (!cancelled) setProxies(res.proxies ?? []);
      })
      .catch(() => {
        if (!cancelled) setProxies([]);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const now = Date.now();
  const stats = useMemo(() => {
    const expired = accounts.filter(
      (account) => workBuddyExpiryState(account.expires_at, now) === "expired",
    ).length;
    const enabled = accounts.filter((account) => account.enabled !== false).length;
    return { total: accounts.length, enabled, expired };
  }, [accounts, now]);

  const filtered = useMemo(() => {
    const keyword = searchQuery.trim().toLowerCase();
    return accounts.filter((account) => {
      if (statusFilter === "enabled" && account.enabled === false) return false;
      if (statusFilter === "disabled" && account.enabled !== false) return false;
      if (statusFilter === "expired" && workBuddyExpiryState(account.expires_at, now) !== "expired") {
        return false;
      }
      if (!keyword) return true;
      return [account.name, account.uid, account.nickname, account.realm, ...(account.models ?? [])]
        .filter(Boolean)
        .some((value) => String(value).toLowerCase().includes(keyword));
    });
  }, [accounts, now, searchQuery, statusFilter]);

  const totalPages = Math.max(1, Math.ceil(filtered.length / pageSize));
  useEffect(() => {
    if (page > totalPages) setPage(totalPages);
  }, [page, totalPages]);
  const pagedAccounts = useMemo(
    () => filtered.slice((page - 1) * pageSize, page * pageSize),
    [filtered, page, pageSize],
  );

  const pageSelected = pagedAccounts.length > 0 && pagedAccounts.every((account) => selected.has(account.id));
  const toggleSelectAll = () => {
    setSelected((current) => {
      const next = new Set(current);
      if (pageSelected) {
        for (const account of pagedAccounts) next.delete(account.id);
      } else {
        for (const account of pagedAccounts) next.add(account.id);
      }
      return next;
    });
  };
  const toggleSelect = (id: number) => {
    setSelected((current) => {
      const next = new Set(current);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  const patchAccount = (id: number, patch: Partial<WorkBuddyAccount>) => {
    setAccounts((current) => current.map((account) => (account.id === id ? { ...account, ...patch } : account)));
  };

  const handleToggleEnabled = async (account: WorkBuddyAccount) => {
    const next = account.enabled === false;
    setBusy({ id: account.id, action: "toggle" });
    patchAccount(account.id, { enabled: next });
    try {
      await api.toggleAccountEnabled(account.id, next);
      showToast(next ? t("workbuddy.enabledToast") : t("workbuddy.disabledToast"), "success");
      patchAccount(account.id, { enabled: next, status: next ? "active" : "paused" });
    } catch (toggleError) {
      patchAccount(account.id, { enabled: !next });
      showToast(getErrorMessage(toggleError), "error");
    } finally {
      setBusy(null);
    }
  };

  const handleTest = async (account: WorkBuddyAccount) => {
    setBusy({ id: account.id, action: "test" });
    try {
      const result = await api.testWorkBuddyAccount(account.id);
      if (result.ok) {
        const suffix = result.model ? ` · ${result.model}` : "";
        showToast(
          `${workBuddyAccountLabel(account)} · ${t("workbuddy.testOk", {
            latency: result.latency_ms ?? 0,
          })}${suffix}`,
          "success",
        );
      } else {
        showToast(`${workBuddyAccountLabel(account)} · ${result.message || t("workbuddy.testFailed")}`, "error");
      }
    } catch (testError) {
      showToast(getErrorMessage(testError), "error");
    } finally {
      setBusy(null);
    }
  };

  const handleRefreshToken = async (account: WorkBuddyAccount) => {
    setBusy({ id: account.id, action: "refresh" });
    try {
      const result = await api.refreshWorkBuddyAccount(account.id);
      if (result.ok) {
        patchAccount(account.id, { expires_at: result.expires_at ?? account.expires_at });
        showToast(t("workbuddy.refreshOk"), "success");
      } else {
        showToast(result.message || t("workbuddy.refreshFailed"), "error");
      }
    } catch (refreshError) {
      showToast(getErrorMessage(refreshError), "error");
    } finally {
      setBusy(null);
    }
  };

  const handleDelete = async (ids: number[]) => {
    if (ids.length === 0) return;
    const confirmed = await confirm({
      title: t("workbuddy.deleteTitle"),
      description: t("workbuddy.deleteDesc", { count: ids.length }),
      confirmText: t("common.delete"),
      tone: "destructive",
      confirmVariant: "destructive",
    });
    if (!confirmed) return;
    setBatchBusy(true);
    try {
      const response = await api.deleteWorkBuddyAccounts(ids);
      // 后端 deleted 是"实际删除成功的 id 列表"，用长度做计数。
      const removed = response.deleted?.length ?? ids.length;
      showToast(t("workbuddy.deleted", { count: removed }), "success");
      setSelected((current) => {
        const next = new Set(current);
        for (const id of ids) next.delete(id);
        return next;
      });
      await reload({ silent: true });
    } catch (deleteError) {
      showToast(getErrorMessage(deleteError), "error");
    } finally {
      setBatchBusy(false);
    }
  };

  const rowBusyAction = (id: number): RowAction | null => (busy?.id === id ? busy.action : null);

  return (
    <div className="relative @container/accounts">
      {confirmDialog}
      <StateShell
        variant="page"
        loading={loading}
        error={error}
        onRetry={() => void reload()}
        isEmpty={!loading && !error && accounts.length === 0}
        loadingTitle={t("workbuddy.loadingTitle")}
        loadingDescription={t("workbuddy.loadingDesc")}
        errorTitle={t("workbuddy.errorTitle")}
        emptyTitle={t("workbuddy.emptyTitle")}
        emptyDescription={t("workbuddy.emptyDesc")}
        emptyIcon={<ChannelLogo channel="workbuddy" size={28} />}
        action={
          <div className="flex flex-wrap items-center justify-center gap-1.5">
            <Button onClick={() => setShowAdd(true)}>
              <Plus className="size-3.5" />
              {t("workbuddy.addAccount")}
            </Button>
            <Button variant="outline" onClick={() => setShowImport(true)}>
              <Upload className="size-3.5" />
              {t("workbuddy.importTitle")}
            </Button>
          </div>
        }
      >
        <PageHeader
          title={t("workbuddy.pageTitle")}
          description={t("workbuddy.pageSubtitle")}
          onRefresh={() => void reload()}
          hideTitle
          actionsBelow
          titleAdornment={headerSlot}
          actions={
            <div className="flex flex-wrap items-center gap-1.5">
              <Button size="sm" onClick={() => setShowAdd(true)}>
                <Plus className="size-3.5" />
                {t("workbuddy.addAccount")}
              </Button>
              <Button variant="outline" size="sm" onClick={() => setShowImport(true)}>
                <Upload className="size-3.5" />
                <span className="hidden sm:inline">{t("workbuddy.importTitle")}</span>
              </Button>
              {selected.size > 0 ? (
                <Button
                  variant="outline"
                  size="sm"
                  className="text-destructive hover:bg-destructive/10 hover:text-destructive"
                  disabled={batchBusy}
                  onClick={() => void handleDelete(Array.from(selected))}
                >
                  {batchBusy ? <Loader2 className="size-3.5 animate-spin" /> : <Trash2 className="size-3.5" />}
                  {t("workbuddy.batchDelete")}
                </Button>
              ) : null}
            </div>
          }
        />

        {/* 端点范围提示：接 Responses/Anthropic 协议的客户端会 400，先讲清楚避免误接。 */}
        <div className="mb-3 flex items-start gap-2.5 rounded-xl border border-sky-500/30 bg-sky-500/[0.06] px-3 py-2.5">
          <ServerCog className="mt-0.5 size-4 shrink-0 text-sky-600 dark:text-sky-300" aria-hidden />
          <div className="min-w-0">
            <div className="text-[12px] font-semibold text-foreground">{t("workbuddy.endpointNoticeTitle")}</div>
            <p className="mt-0.5 text-[11px] leading-relaxed text-muted-foreground">
              {t("workbuddy.endpointNotice")}
            </p>
          </div>
        </div>

        <div className="mb-4 grid grid-cols-3 gap-2 sm:gap-3">
          <CompactStat
            label={t("workbuddy.statTotal")}
            chipLabel={t("workbuddy.filterAll")}
            value={stats.total}
            tone="neutral"
            active={statusFilter === "all"}
            onClick={() => setStatusFilter("all")}
          />
          <CompactStat
            label={t("workbuddy.statEnabled")}
            chipLabel={t("workbuddy.filterEnabled")}
            value={stats.enabled}
            tone="success"
            active={statusFilter === "enabled"}
            onClick={() => setStatusFilter(statusFilter === "enabled" ? "all" : "enabled")}
          />
          <CompactStat
            label={t("workbuddy.statExpired")}
            chipLabel={t("workbuddy.filterExpired")}
            value={stats.expired}
            tone={stats.expired > 0 ? "danger" : "neutral"}
            active={statusFilter === "expired"}
            onClick={() => setStatusFilter(statusFilter === "expired" ? "all" : "expired")}
          />
        </div>

        <div className="toolbar-surface mb-3 flex flex-col gap-2.5">
          <div className="flex flex-col gap-2 sm:flex-row sm:flex-wrap sm:items-center">
            <div className="relative w-full shrink-0 sm:w-64">
              <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
              <Input
                className="h-9 rounded-lg pl-9 text-[13px] sm:h-8"
                placeholder={t("workbuddy.searchPlaceholder")}
                value={searchQuery}
                onChange={(event) => {
                  setSearchQuery(event.target.value);
                  setPage(1);
                }}
              />
            </div>
            <SegmentedPillGroup<StatusFilter>
              value={statusFilter}
              onChange={(next) => {
                setStatusFilter(next);
                setPage(1);
              }}
              label={t("workbuddy.filterAll")}
              className="w-auto"
              options={[
                { value: "all", label: t("workbuddy.filterAll") },
                { value: "enabled", label: t("workbuddy.filterEnabled") },
                { value: "disabled", label: t("workbuddy.filterDisabled") },
                { value: "expired", label: t("workbuddy.filterExpired") },
              ]}
            />
            {selected.size > 0 ? (
              <span className="text-[12px] font-medium text-muted-foreground">
                {t("workbuddy.selectedCount", { count: selected.size })}
              </span>
            ) : null}
          </div>
        </div>

        {filtered.length === 0 ? (
          <div className="flex flex-col items-center gap-2 rounded-2xl border border-border/80 bg-card/80 px-6 py-10 text-center">
            <span className="flex size-12 items-center justify-center rounded-xl bg-muted/60 text-muted-foreground">
              <Search className="size-5" />
            </span>
            <strong className="text-sm font-semibold text-foreground">{t("workbuddy.noMatchTitle")}</strong>
            <p className="text-xs text-muted-foreground">{t("workbuddy.noMatchDesc")}</p>
            <Button
              variant="outline"
              size="sm"
              onClick={() => {
                setSearchQuery("");
                setStatusFilter("all");
                setPage(1);
              }}
            >
              {t("workbuddy.clearFilters")}
            </Button>
          </div>
        ) : (
          <>
            <div className="data-table-shell overflow-x-auto">
              <Table className="[&_td]:px-2.5 [&_th]:px-2.5 [&_td]:py-3">
                <TableHeader>
                  <TableRow>
                    <TableHead className="w-9">
                      <input
                        type="checkbox"
                        className="size-4 cursor-pointer rounded border-border accent-primary"
                        aria-label={t("workbuddy.selectAll")}
                        title={t("workbuddy.selectAll")}
                        checked={pageSelected}
                        onChange={toggleSelectAll}
                      />
                    </TableHead>
                    <TableHead className="text-[13px] font-semibold">{t("workbuddy.colAccount")}</TableHead>
                    <TableHead className="text-[13px] font-semibold">{t("workbuddy.colUid")}</TableHead>
                    <TableHead className="text-[13px] font-semibold">{t("workbuddy.colNickname")}</TableHead>
                    <TableHead className="text-[13px] font-semibold">{t("workbuddy.colRealm")}</TableHead>
                    <TableHead className="text-[13px] font-semibold">{t("workbuddy.colStatus")}</TableHead>
                    <TableHead className="text-[13px] font-semibold">{t("workbuddy.colModels")}</TableHead>
                    <TableHead className="text-[13px] font-semibold">{t("workbuddy.colExpires")}</TableHead>
                    <TableHead className="text-right text-[13px] font-semibold">{t("workbuddy.colActions")}</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {pagedAccounts.map((account) => (
                    <WorkBuddyRow
                      key={account.id}
                      account={account}
                      selected={selected.has(account.id)}
                      busyAction={rowBusyAction(account.id)}
                      onToggleSelect={() => toggleSelect(account.id)}
                      onToggleEnabled={() => void handleToggleEnabled(account)}
                      onTest={() => void handleTest(account)}
                      onRefresh={() => void handleRefreshToken(account)}
                      onEdit={() => setEditAccount(account)}
                      onDelete={() => void handleDelete([account.id])}
                    />
                  ))}
                </TableBody>
              </Table>
            </div>
            <Pagination
              page={page}
              totalPages={totalPages}
              onPageChange={setPage}
              totalItems={filtered.length}
              pageSize={pageSize}
              onPageSizeChange={setPageSize}
              pageSizeOptions={DEFAULT_PAGE_SIZE_OPTIONS}
            />
          </>
        )}
      </StateShell>

      <WorkBuddyAddModal
        show={showAdd}
        proxies={proxies}
        onClose={() => setShowAdd(false)}
        onAdded={() => reload({ silent: true })}
      />
      <WorkBuddyImportModal
        show={showImport}
        onClose={() => setShowImport(false)}
        onImported={() => reload({ silent: true })}
      />
      <WorkBuddyEditModal
        account={editAccount}
        proxies={proxies}
        onClose={() => setEditAccount(null)}
        onSaved={() => reload({ silent: true })}
      />
    </div>
  );
}

const WorkBuddyRow = memo(function WorkBuddyRow({
  account,
  selected,
  busyAction,
  onToggleSelect,
  onToggleEnabled,
  onTest,
  onRefresh,
  onEdit,
  onDelete,
}: {
  account: WorkBuddyAccount;
  selected: boolean;
  busyAction: RowAction | null;
  onToggleSelect: () => void;
  onToggleEnabled: () => void;
  onTest: () => void;
  onRefresh: () => void;
  onEdit: () => void;
  onDelete: () => void;
}) {
  const { t } = useTranslation();
  const enabled = account.enabled !== false;
  const expiry = workBuddyExpiryState(account.expires_at);
  const models = account.models ?? [];
  const label = workBuddyAccountLabel(account);
  const displayStatus = enabled ? account.status || "active" : "paused";

  return (
    <TableRow className={cn(selected && "bg-primary/5", !enabled && "opacity-70")}>
      <TableCell className="w-9">
        <input
          type="checkbox"
          className="size-4 cursor-pointer rounded border-border accent-primary"
          aria-label={label}
          checked={selected}
          onChange={onToggleSelect}
        />
      </TableCell>
      <TableCell>
        <div className="flex min-w-0 items-center gap-2.5">
          <ChannelLogo channel="workbuddy" size={26} />
          <div className="min-w-0">
            <div className="max-w-[200px] truncate text-[13px] font-semibold text-foreground" title={label}>
              {label}
            </div>
            {account.last_error ? (
              <div
                className="max-w-[220px] truncate text-[11px] text-destructive/80"
                title={account.last_error}
              >
                {account.last_error}
              </div>
            ) : null}
          </div>
        </div>
      </TableCell>
      <TableCell>
        <span className="font-mono text-[12px] text-muted-foreground" title={account.uid}>
          {account.uid || "—"}
        </span>
      </TableCell>
      <TableCell>
        <span className="max-w-[140px] truncate text-[12px] text-muted-foreground" title={account.nickname}>
          {account.nickname || "—"}
        </span>
      </TableCell>
      <TableCell>
        <span className="text-[12px] text-muted-foreground">{realmLabel(account.realm, t)}</span>
      </TableCell>
      <TableCell>
        <div className="flex items-center gap-2">
          <StatusBadge status={displayStatus} errorMessage={account.last_error || undefined} />
          <span title={enabled ? t("workbuddy.disable") : t("workbuddy.enable")} className="inline-flex">
            <Switch
              checked={enabled}
              disabled={busyAction === "toggle"}
              onCheckedChange={onToggleEnabled}
              aria-label={t("workbuddy.colStatus")}
              className={TOGGLE_ON}
            />
          </span>
        </div>
      </TableCell>
      <TableCell>
        {models.length === 0 ? (
          <span className="text-[12px] text-muted-foreground/70">{t("workbuddy.modelsEmpty")}</span>
        ) : (
          <div className="flex max-w-[150px] flex-wrap items-center gap-1">
            <span className="rounded-md bg-muted/50 px-1.5 py-0.5 text-[10px] font-semibold tabular-nums text-muted-foreground ring-1 ring-inset ring-border">
              {models.length}
            </span>
            {models.slice(0, 2).map((model) => (
              <span
                key={model}
                className="max-w-[9rem] truncate rounded-md bg-muted/50 px-1.5 py-0.5 font-mono text-[10px] text-muted-foreground ring-1 ring-inset ring-border"
                title={model}
              >
                {model}
              </span>
            ))}
            {models.length > 2 ? (
              <span className="text-[10px] font-medium text-muted-foreground" title={models.join(", ")}>
                +{models.length - 2}
              </span>
            ) : null}
          </div>
        )}
      </TableCell>
      <TableCell>
        <div className={cn(EXPIRY_CLASS, expiryTone(expiry))}>
          {expiry === "unset" ? (
            t("workbuddy.noExpiry")
          ) : expiry === "expired" ? (
            t("workbuddy.statusExpired")
          ) : (
            // 过期时间是未来时间点：显示绝对时间（跟随时区设置），相对时间对"还有几天到期"没有意义。
            <span title={account.expires_at}>{formatBeijingTime(account.expires_at)}</span>
          )}
        </div>
      </TableCell>
      <TableCell className="text-right">
        <div className="inline-flex items-center gap-0.5">
          <Button
            variant="ghost"
            size="icon-sm"
            title={t("workbuddy.testConnection")}
            aria-label={t("workbuddy.testConnection")}
            disabled={busyAction !== null}
            onClick={onTest}
          >
            {busyAction === "test" ? (
              <Loader2 className="size-3.5 animate-spin" />
            ) : (
              <FlaskConical className="size-3.5" />
            )}
          </Button>
          <Button
            variant="ghost"
            size="icon-sm"
            title={t("workbuddy.refreshTokenAction")}
            aria-label={t("workbuddy.refreshTokenAction")}
            disabled={busyAction !== null}
            onClick={onRefresh}
          >
            <RefreshCw className={cn("size-3.5", busyAction === "refresh" && "animate-spin")} />
          </Button>
          <Button
            variant="ghost"
            size="icon-sm"
            title={t("workbuddy.editTitle")}
            aria-label={t("workbuddy.editTitle")}
            onClick={onEdit}
          >
            <Pencil className="size-3.5" />
          </Button>
          <Button
            variant="ghost"
            size="icon-sm"
            className="text-muted-foreground hover:text-destructive"
            title={t("workbuddy.deleteAction")}
            aria-label={t("workbuddy.deleteAction")}
            disabled={busyAction !== null}
            onClick={onDelete}
          >
            <Trash2 className="size-3.5" />
          </Button>
        </div>
      </TableCell>
    </TableRow>
  );
});

export default WorkBuddyAccounts;
