import { useCallback, useState } from "react";
import { useTranslation } from "react-i18next";
import { Info, Loader2, Plus } from "lucide-react";
import { api } from "../../api";
import type { ProxyRow } from "../../api";
import type { AddWorkBuddyAccountRequest } from "../../types";
import Modal from "../../components/Modal";
import { ProxyField } from "../../components/ProxyField";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { useToast } from "../../hooks/useToast";
import { parseWorkBuddyModelLines } from "../../lib/workbuddyImport";
import { getErrorMessage } from "../../utils/error";
import {
  dateTimeLocalToISO,
  FieldHint,
  FieldLabel,
  SecretInput,
  TEXTAREA_CLASS,
} from "./WorkBuddyFormFields";

interface AddDraft {
  name: string;
  uid: string;
  realm: string;
  accessToken: string;
  refreshToken: string;
  expiresAt: string;
  models: string;
  proxyUrl: string;
}

const EMPTY_ADD_DRAFT: AddDraft = {
  name: "",
  uid: "",
  realm: "global",
  accessToken: "",
  refreshToken: "",
  expiresAt: "",
  models: "",
  proxyUrl: "",
};

/** 添加单个 WorkBuddy(wb2api) 账号：access token 必填，其余可留空由后端兜底。 */
export default function WorkBuddyAddModal({
  show,
  proxies,
  onClose,
  onAdded,
}: {
  show: boolean;
  proxies: ProxyRow[];
  onClose: () => void;
  onAdded: () => Promise<void> | void;
}) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const [draft, setDraft] = useState<AddDraft>(EMPTY_ADD_DRAFT);
  const [submitting, setSubmitting] = useState(false);

  const realmOptions = [
    { label: t("workbuddy.realmGlobal"), value: "global" },
    { label: t("workbuddy.realmCN"), value: "cn" },
  ];

  const close = useCallback(() => {
    if (submitting) return;
    onClose();
    setDraft(EMPTY_ADD_DRAFT);
  }, [onClose, submitting]);

  const canSubmit = Boolean(draft.uid.trim() && draft.accessToken.trim());

  const handleSubmit = async () => {
    if (submitting || !canSubmit) return;
    const payload: AddWorkBuddyAccountRequest = {
      uid: draft.uid.trim(),
      access_token: draft.accessToken.trim(),
      realm: draft.realm,
    };
    const name = draft.name.trim();
    const refreshToken = draft.refreshToken.trim();
    const expiresAt = dateTimeLocalToISO(draft.expiresAt);
    const proxyUrl = draft.proxyUrl.trim();
    const models = parseWorkBuddyModelLines(draft.models);
    if (name) payload.name = name;
    if (refreshToken) payload.refresh_token = refreshToken;
    if (expiresAt) payload.expires_at = expiresAt;
    if (proxyUrl) payload.proxy_url = proxyUrl;
    if (models.length > 0) payload.models = models;

    setSubmitting(true);
    try {
      const response = await api.addWorkBuddyAccount(payload);
      const warning = response.warning?.trim();
      showToast(warning || t("workbuddy.addSuccess"), warning ? "warning" : "success");
      onClose();
      setDraft(EMPTY_ADD_DRAFT);
      await onAdded();
    } catch (addError) {
      showToast(getErrorMessage(addError), "error");
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Modal
      show={show}
      title={t("workbuddy.addTitle")}
      onClose={close}
      contentClassName="sm:max-w-[620px]"
      footer={
        <>
          <Button variant="outline" onClick={close} disabled={submitting}>
            {t("common.cancel")}
          </Button>
          <Button onClick={() => void handleSubmit()} disabled={submitting || !canSubmit}>
            {submitting ? <Loader2 className="animate-spin" /> : <Plus />}
            {submitting ? t("workbuddy.adding") : t("workbuddy.addAccount")}
          </Button>
        </>
      }
    >
      <div className="space-y-4">
        <div className="flex items-start gap-2 rounded-xl border border-border/70 bg-muted/30 px-3 py-2.5 text-[11px] leading-relaxed text-muted-foreground">
          <Info className="mt-0.5 size-3.5 shrink-0" aria-hidden />
          <span>{t("workbuddy.addDesc")}</span>
        </div>

        <div className="grid gap-3 sm:grid-cols-2">
          <label className="block space-y-1.5">
            <FieldLabel>{t("workbuddy.name")}</FieldLabel>
            <Input
              value={draft.name}
              onChange={(event) => setDraft((current) => ({ ...current, name: event.target.value }))}
              placeholder={t("workbuddy.namePlaceholder")}
            />
          </label>
          <label className="block space-y-1.5">
            <FieldLabel>{t("workbuddy.uid")}</FieldLabel>
            <Input
              value={draft.uid}
              onChange={(event) => setDraft((current) => ({ ...current, uid: event.target.value }))}
              placeholder={t("workbuddy.uidPlaceholder")}
              spellCheck={false}
              className="font-mono"
            />
          </label>
        </div>

        <label className="block space-y-1.5">
          <FieldLabel>{t("workbuddy.realm")}</FieldLabel>
          <Select
            aria-label={t("workbuddy.realm")}
            value={draft.realm}
            onValueChange={(realm) => setDraft((current) => ({ ...current, realm }))}
            options={realmOptions}
            compact
            className="w-auto min-w-[160px]"
          />
          <FieldHint>{t("workbuddy.realmHint")}</FieldHint>
        </label>

        <label className="block space-y-1.5">
          <FieldLabel>{t("workbuddy.accessToken")}</FieldLabel>
          <SecretInput
            value={draft.accessToken}
            onChange={(accessToken) => setDraft((current) => ({ ...current, accessToken }))}
            placeholder={t("workbuddy.accessTokenPlaceholder")}
          />
        </label>

        <div className="grid gap-3 sm:grid-cols-2">
          <label className="block space-y-1.5">
            <FieldLabel>{t("workbuddy.refreshToken")}</FieldLabel>
            <SecretInput
              value={draft.refreshToken}
              onChange={(refreshToken) => setDraft((current) => ({ ...current, refreshToken }))}
              placeholder={t("workbuddy.refreshTokenPlaceholder")}
            />
          </label>
          <label className="block space-y-1.5">
            <FieldLabel>{t("workbuddy.expiresAt")}</FieldLabel>
            <Input
              type="datetime-local"
              value={draft.expiresAt}
              onChange={(event) => setDraft((current) => ({ ...current, expiresAt: event.target.value }))}
            />
            <FieldHint>{t("workbuddy.expiresAtHint")}</FieldHint>
          </label>
        </div>

        <label className="block space-y-1.5">
          <FieldLabel>{t("workbuddy.models")}</FieldLabel>
          <textarea
            value={draft.models}
            onChange={(event) => setDraft((current) => ({ ...current, models: event.target.value }))}
            rows={4}
            spellCheck={false}
            placeholder={t("workbuddy.modelsPlaceholder")}
            className={TEXTAREA_CLASS}
          />
          <FieldHint>{t("workbuddy.syncModelsHint")}</FieldHint>
        </label>

        <ProxyField
          value={draft.proxyUrl}
          onChange={(proxyUrl) => setDraft((current) => ({ ...current, proxyUrl }))}
          proxies={proxies}
        />
      </div>
    </Modal>
  );
}
