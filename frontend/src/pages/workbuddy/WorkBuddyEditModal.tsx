import { useCallback, useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { Loader2, RefreshCw, Save } from "lucide-react";
import { api } from "../../api";
import type { ProxyRow } from "../../api";
import type { UpdateWorkBuddyAccountRequest, WorkBuddyAccount } from "../../types";
import Modal from "../../components/Modal";
import { ProxyField } from "../../components/ProxyField";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { useToast } from "../../hooks/useToast";
import { parseWorkBuddyModelLines, workBuddyAccountLabel } from "../../lib/workbuddyImport";
import { getErrorMessage } from "../../utils/error";
import {
  dateTimeLocalToISO,
  FieldHint,
  FieldLabel,
  isoToDateTimeLocal,
  SecretInput,
  TEXTAREA_CLASS,
} from "./WorkBuddyFormFields";

interface EditDraft {
  name: string;
  proxyUrl: string;
  accessToken: string;
  refreshToken: string;
  expiresAt: string;
  models: string;
}

function draftFromAccount(account: WorkBuddyAccount): EditDraft {
  return {
    name: account.name ?? "",
    proxyUrl: account.proxy_url ?? "",
    accessToken: "",
    refreshToken: "",
    expiresAt: isoToDateTimeLocal(account.expires_at),
    models: (account.models ?? []).join("\n"),
  };
}

/**
 * 编辑 WorkBuddy 账号：名称 / 代理 / 凭据 / 过期时间 / 模型白名单统一走
 * PATCH /accounts/{id}/workbuddy（模型必须走这条：通用 /accounts/{id}/models 对中转账号返回 400）。
 */
export default function WorkBuddyEditModal({
  account,
  proxies,
  onClose,
  onSaved,
}: {
  account: WorkBuddyAccount | null;
  proxies: ProxyRow[];
  onClose: () => void;
  onSaved: () => Promise<void> | void;
}) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const [draft, setDraft] = useState<EditDraft | null>(null);
  const [saving, setSaving] = useState(false);
  const [syncing, setSyncing] = useState(false);
  // 同步上游模型会直接写回账号，这里记住"服务端当前值"，避免保存时重复提交同一份列表。
  const [serverModels, setServerModels] = useState("");

  useEffect(() => {
    setDraft(account ? draftFromAccount(account) : null);
    setServerModels((account?.models ?? []).join(","));
  }, [account]);

  const close = useCallback(() => {
    if (saving) return;
    onClose();
  }, [onClose, saving]);

  const handleSyncModels = async () => {
    if (!account || syncing) return;
    setSyncing(true);
    try {
      const response = await api.syncWorkBuddyModels([account.id]);
      const models = response.models ?? [];
      setDraft((current) => (current ? { ...current, models: models.join("\n") } : current));
      setServerModels(models.join(","));
      const failures = response.errors?.length ?? 0;
      showToast(
        failures > 0
          ? `${t("workbuddy.syncModelsSuccess", { count: models.length })} · ${response.errors?.[0]}`
          : t("workbuddy.syncModelsSuccess", { count: models.length }),
        failures > 0 ? "warning" : "success",
      );
    } catch (syncError) {
      showToast(`${t("workbuddy.syncModelsFailed")}: ${getErrorMessage(syncError)}`, "error");
    } finally {
      setSyncing(false);
    }
  };

  const handleSubmit = async () => {
    if (!account || !draft || saving) return;
    const payload: UpdateWorkBuddyAccountRequest = {};
    const name = draft.name.trim();
    if (name !== (account.name ?? "")) payload.name = name;
    const proxyUrl = draft.proxyUrl.trim();
    if (proxyUrl) payload.proxy_url = proxyUrl;
    const accessToken = draft.accessToken.trim();
    if (accessToken) payload.access_token = accessToken;
    const refreshToken = draft.refreshToken.trim();
    if (refreshToken) payload.refresh_token = refreshToken;
    const expiresAt = dateTimeLocalToISO(draft.expiresAt);
    if (expiresAt && expiresAt !== (account.expires_at ?? "")) payload.expires_at = expiresAt;

    // 模型白名单必须走 workbuddy 专属 PATCH：通用 /accounts/{id}/models 会拒绝中转账号
    // （返回 400「中转/Grok 账号请在账号设置中编辑模型列表」）。
    const models = parseWorkBuddyModelLines(draft.models);
    if (models.join(",") !== serverModels) payload.models = models;

    if (Object.keys(payload).length === 0) {
      close();
      return;
    }

    setSaving(true);
    try {
      const warnings: string[] = [];
      const response = await api.updateWorkBuddyAccount(account.id, payload);
      if (response.warning?.trim()) warnings.push(response.warning.trim());
      showToast(warnings.join(" ") || t("workbuddy.editSuccess"), warnings.length > 0 ? "warning" : "success");
      onClose();
      await onSaved();
    } catch (editError) {
      showToast(getErrorMessage(editError), "error");
    } finally {
      setSaving(false);
    }
  };

  return (
    <Modal
      show={account !== null && draft !== null}
      title={
        <span className="flex min-w-0 flex-col gap-0.5">
          <span className="truncate">{t("workbuddy.editTitle")}</span>
          <span className="truncate font-mono text-[11px] font-normal text-muted-foreground">
            {account ? workBuddyAccountLabel(account) : ""}
          </span>
        </span>
      }
      onClose={close}
      contentClassName="sm:max-w-[620px]"
      footer={
        <>
          <Button variant="outline" onClick={close} disabled={saving}>
            {t("common.cancel")}
          </Button>
          <Button onClick={() => void handleSubmit()} disabled={saving || !draft}>
            {saving ? <Loader2 className="animate-spin" /> : <Save />}
            {saving ? t("common.saving") : t("common.save")}
          </Button>
        </>
      }
    >
      {draft ? (
        <div className="space-y-4">
          <p className="text-[11px] leading-relaxed text-muted-foreground">{t("workbuddy.editDesc")}</p>

          <div className="grid gap-3 sm:grid-cols-2">
            <label className="block space-y-1.5">
              <FieldLabel>{t("workbuddy.name")}</FieldLabel>
              <Input
                value={draft.name}
                onChange={(event) => setDraft((current) => (current ? { ...current, name: event.target.value } : current))}
                placeholder={t("workbuddy.namePlaceholder")}
              />
            </label>
            <label className="block space-y-1.5">
              <FieldLabel>{t("workbuddy.expiresAt")}</FieldLabel>
              <Input
                type="datetime-local"
                value={draft.expiresAt}
                onChange={(event) =>
                  setDraft((current) => (current ? { ...current, expiresAt: event.target.value } : current))
                }
              />
            </label>
          </div>

          <div className="grid gap-3 sm:grid-cols-2">
            <label className="block space-y-1.5">
              <FieldLabel>{t("workbuddy.accessToken")}</FieldLabel>
              <SecretInput
                value={draft.accessToken}
                onChange={(accessToken) => setDraft((current) => (current ? { ...current, accessToken } : current))}
                placeholder={t("workbuddy.accessTokenPlaceholder")}
              />
            </label>
            <label className="block space-y-1.5">
              <FieldLabel>{t("workbuddy.refreshToken")}</FieldLabel>
              <SecretInput
                value={draft.refreshToken}
                onChange={(refreshToken) => setDraft((current) => (current ? { ...current, refreshToken } : current))}
                placeholder={t("workbuddy.refreshTokenPlaceholder")}
              />
            </label>
          </div>

          <div className="space-y-1.5">
            <div className="flex items-center justify-between gap-2">
              <FieldLabel>{t("workbuddy.models")}</FieldLabel>
              <Button variant="outline" size="sm" disabled={syncing} onClick={() => void handleSyncModels()}>
                <RefreshCw className={syncing ? "size-3.5 animate-spin" : "size-3.5"} />
                {syncing ? t("workbuddy.syncingModels") : t("workbuddy.syncModels")}
              </Button>
            </div>
            <textarea
              value={draft.models}
              onChange={(event) =>
                setDraft((current) => (current ? { ...current, models: event.target.value } : current))
              }
              rows={5}
              spellCheck={false}
              placeholder={t("workbuddy.modelsPlaceholder")}
              className={TEXTAREA_CLASS}
            />
            <FieldHint>{t("workbuddy.syncModelsHint")}</FieldHint>
            <FieldHint>{t("workbuddy.syncModelsWrites")}</FieldHint>
          </div>

          <ProxyField
            value={draft.proxyUrl}
            onChange={(proxyUrl) => setDraft((current) => (current ? { ...current, proxyUrl } : current))}
            proxies={proxies}
            label={t("accounts.proxyUrl")}
          />
        </div>
      ) : null}
    </Modal>
  );
}
