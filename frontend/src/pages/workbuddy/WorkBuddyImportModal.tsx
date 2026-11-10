import { useCallback, useRef, useState } from "react";
import type { ChangeEvent, DragEvent } from "react";
import { useTranslation } from "react-i18next";
import {
  CheckCircle2,
  ClipboardPaste,
  FileJson,
  FolderOpen,
  Loader2,
  TriangleAlert,
  Upload,
  X,
} from "lucide-react";
import { api } from "../../api";
import type { WorkBuddyImportItem } from "../../types";
import Modal from "../../components/Modal";
import { Button } from "@/components/ui/button";
import { SegmentedPillGroup } from "@/components/ui/segmented-pill-group";
import { useToast } from "../../hooks/useToast";
import { cn } from "@/lib/utils";
import { mergeWorkBuddyImportItems, parseWorkBuddyAuthText } from "../../lib/workbuddyImport";
import { getErrorMessage } from "../../utils/error";
import { FieldHint, FieldLabel, TEXTAREA_CLASS } from "./WorkBuddyFormFields";

type ImportMode = "files" | "paste";

interface CredentialFile {
  name: string;
  size: number;
  content: string;
}

function formatFileSize(bytes: number): string {
  if (bytes >= 1024 * 1024) return `${(bytes / 1024 / 1024).toFixed(1)} MB`;
  if (bytes >= 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${bytes} B`;
}

/**
 * 批量导入 WorkBuddy 账号：支持 wb2api 的 auths/workbuddy-<uid>.json
 * （单个对象、数组或 {accounts:[…]} 包装），可多选文件或直接粘贴原文。
 * 前端只做预检与预览，原文仍交给后端解析。
 */
export default function WorkBuddyImportModal({
  show,
  onClose,
  onImported,
}: {
  show: boolean;
  onClose: () => void;
  onImported: () => Promise<void> | void;
}) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const fileInputRef = useRef<HTMLInputElement | null>(null);
  const [mode, setMode] = useState<ImportMode>("files");
  const [files, setFiles] = useState<CredentialFile[]>([]);
  const [pasted, setPasted] = useState("");
  const [dragging, setDragging] = useState(false);
  const [importing, setImporting] = useState(false);

  const reset = useCallback(() => {
    setMode("files");
    setFiles([]);
    setPasted("");
    setDragging(false);
    if (fileInputRef.current) fileInputRef.current.value = "";
  }, []);

  const close = useCallback(() => {
    if (importing) return;
    onClose();
    reset();
  }, [importing, onClose, reset]);

  // 文件与粘贴内容统一进同一个解析器，预览数量与提交内容保持一致。
  const parsedFiles = files.map((file) => parseWorkBuddyAuthText(file.content));
  const items: WorkBuddyImportItem[] =
    mode === "files"
      ? mergeWorkBuddyImportItems(...parsedFiles.map((result) => result.items))
      : parseWorkBuddyAuthText(pasted).items;
  const skipped =
    mode === "files"
      ? parsedFiles.reduce((sum, result) => sum + result.skipped, 0)
      : parseWorkBuddyAuthText(pasted).skipped;
  const rawText = mode === "files" ? files.map((file) => file.content).join("\n") : pasted.trim();

  const addFiles = async (list: FileList | File[]) => {
    const picked = Array.from(list).filter(
      (file) => file.name.toLowerCase().endsWith(".json") || file.type === "application/json",
    );
    if (picked.length === 0) {
      showToast(t("workbuddy.importFailed"), "error");
      return;
    }
    try {
      const contents = await Promise.all(
        picked.map(async (file) => ({ name: file.name, size: file.size, content: await file.text() })),
      );
      setFiles((current) => {
        const byName = new Map(current.map((file) => [file.name, file]));
        for (const file of contents) byName.set(file.name, file);
        return Array.from(byName.values());
      });
    } catch (fileError) {
      showToast(getErrorMessage(fileError), "error");
    }
  };

  const handleFileInput = async (event: ChangeEvent<HTMLInputElement>) => {
    if (event.target.files?.length) await addFiles(event.target.files);
    event.target.value = "";
  };

  const handleDrop = async (event: DragEvent<HTMLElement>) => {
    event.preventDefault();
    setDragging(false);
    if (event.dataTransfer.files?.length) await addFiles(event.dataTransfer.files);
  };

  const handleImport = async () => {
    if (importing) return;
    if (items.length === 0 && !rawText) {
      showToast(t("workbuddy.importEmpty"), "error");
      return;
    }
    setImporting(true);
    try {
      // 能解析就用结构化条目（多文件合并后的结果）；解析不了就把原文交后端兜底。
      const response = await api.importWorkBuddyAccounts(
        items.length > 0 ? { items } : { text: rawText },
      );
      const imported = response.imported ?? items.length;
      const failed = response.failed ?? 0;
      // 后端逐条失败原因在 errors 里；只要有一条就把它拼进提示，别让用户猜为什么少了几个。
      const firstError = response.errors?.[0]?.trim() ?? "";
      const message = [response.message?.trim() || response.warning?.trim(), firstError]
        .filter(Boolean)
        .join(" · ");
      const failedSome = failed > 0 && imported > 0;
      showToast(
        message || (failedSome
          ? t("workbuddy.importPartial", { imported, failed })
          : t("workbuddy.importDone", { imported })),
        message || failedSome ? "warning" : "success",
      );
      onClose();
      reset();
      await onImported();
    } catch (importError) {
      showToast(getErrorMessage(importError), "error");
    } finally {
      setImporting(false);
    }
  };

  const totalSize = files.reduce((sum, file) => sum + file.size, 0);

  return (
    <Modal
      show={show}
      title={t("workbuddy.importTitle")}
      onClose={close}
      contentClassName="sm:max-w-[640px]"
      footer={
        <>
          <Button variant="outline" onClick={close} disabled={importing}>
            {t("common.cancel")}
          </Button>
          <Button onClick={() => void handleImport()} disabled={importing || items.length === 0}>
            {importing ? <Loader2 className="animate-spin" /> : <Upload />}
            {importing
              ? t("workbuddy.importing")
              : t("workbuddy.importSubmit", { count: items.length })}
          </Button>
        </>
      }
    >
      <div className="space-y-4">
        <p className="text-[11px] leading-relaxed text-muted-foreground">{t("workbuddy.importDesc")}</p>

        <SegmentedPillGroup<ImportMode>
          value={mode}
          onChange={(next) => setMode(next)}
          label={t("workbuddy.importTitle")}
          options={[
            {
              value: "files",
              label: t("workbuddy.importModeFiles"),
              icon: <FolderOpen className="size-3.5" />,
            },
            {
              value: "paste",
              label: t("workbuddy.importModePaste"),
              icon: <ClipboardPaste className="size-3.5" />,
            },
          ]}
        />

        {mode === "files" ? (
          <div className="space-y-2">
            <input
              ref={fileInputRef}
              type="file"
              accept=".json,application/json"
              multiple
              className="hidden"
              onChange={(event) => void handleFileInput(event)}
            />
            <button
              type="button"
              onClick={() => fileInputRef.current?.click()}
              onDragOver={(event) => {
                event.preventDefault();
                setDragging(true);
              }}
              onDragLeave={() => setDragging(false)}
              onDrop={(event) => void handleDrop(event)}
              className={cn(
                "flex w-full flex-col items-center justify-center gap-2 rounded-xl border border-dashed px-5 text-center transition-colors",
                files.length > 0 ? "min-h-[96px] py-4" : "min-h-[150px] py-6",
                dragging
                  ? "border-primary bg-primary/10"
                  : "border-border bg-muted/20 hover:border-primary/40 hover:bg-primary/5",
              )}
            >
              <span className="flex size-10 items-center justify-center rounded-xl bg-primary/10 text-primary">
                <FolderOpen className="size-5" />
              </span>
              <span className="text-sm font-semibold text-foreground">
                {t("workbuddy.importChooseFiles")}
              </span>
              <span className="text-xs text-muted-foreground">{t("workbuddy.importChooseFilesHint")}</span>
            </button>
            {files.length > 0 ? (
              <div className="rounded-xl border border-border">
                <div className="flex items-center justify-between border-b border-border/70 px-3 py-1.5 text-[11px] text-muted-foreground">
                  <span>
                    {t("workbuddy.importFilesSelected", { count: files.length })} · {formatFileSize(totalSize)}
                  </span>
                  <button
                    type="button"
                    className="font-medium transition-colors hover:text-foreground"
                    onClick={() => setFiles([])}
                  >
                    {t("workbuddy.importClearFiles")}
                  </button>
                </div>
                <div className="max-h-36 overflow-y-auto px-1.5 py-1">
                  {files.map((file) => (
                    <div
                      key={file.name}
                      className="flex items-center gap-2 rounded-md px-1.5 py-1 text-xs hover:bg-muted/40"
                    >
                      <FileJson className="size-3.5 shrink-0 text-muted-foreground" />
                      <span className="min-w-0 flex-1 truncate">{file.name}</span>
                      <span className="shrink-0 tabular-nums text-[10px] text-muted-foreground">
                        {formatFileSize(file.size)}
                      </span>
                      <button
                        type="button"
                        className="flex size-5 shrink-0 items-center justify-center rounded text-muted-foreground hover:bg-muted hover:text-foreground"
                        aria-label={file.name}
                        onClick={() => setFiles((current) => current.filter((item) => item.name !== file.name))}
                      >
                        <X className="size-3" />
                      </button>
                    </div>
                  ))}
                </div>
              </div>
            ) : null}
          </div>
        ) : (
          <label className="block space-y-1.5">
            <FieldLabel>{t("workbuddy.importPasteLabel")}</FieldLabel>
            <textarea
              value={pasted}
              onChange={(event) => setPasted(event.target.value)}
              rows={9}
              spellCheck={false}
              placeholder={'{ "account": { "uid": "…" }, "auth": { "accessToken": "…" } }'}
              className={TEXTAREA_CLASS}
            />
            <FieldHint>{t("workbuddy.importPasteHint")}</FieldHint>
          </label>
        )}

        {items.length > 0 ? (
          <div className="rounded-xl border border-emerald-500/30 bg-emerald-500/[0.06] px-3 py-2.5">
            <span className="inline-flex items-center gap-1.5 text-[11px] font-semibold text-emerald-700 dark:text-emerald-300">
              <CheckCircle2 className="size-3.5" aria-hidden />
              {t("workbuddy.importDetected", { count: items.length })}
            </span>
            <div className="mt-1.5 flex max-h-24 flex-wrap gap-1 overflow-y-auto">
              {items.slice(0, 40).map((item) => (
                <span
                  key={item.uid}
                  className="max-w-[12rem] truncate rounded-md bg-background/70 px-1.5 py-0.5 font-mono text-[10px] text-muted-foreground ring-1 ring-inset ring-border"
                  title={`${item.uid}${item.nickname ? ` · ${item.nickname}` : ""}`}
                >
                  {item.nickname || item.uid}
                </span>
              ))}
            </div>
          </div>
        ) : null}

        {skipped > 0 ? (
          <div className="flex items-start gap-1.5 rounded-lg border border-amber-500/30 bg-amber-500/[0.06] px-3 py-2 text-[11px] text-amber-700 dark:text-amber-300">
            <TriangleAlert className="mt-0.5 size-3.5 shrink-0" aria-hidden />
            <span>{t("workbuddy.importSkipped", { count: skipped })}</span>
          </div>
        ) : null}
      </div>
    </Modal>
  );
}
