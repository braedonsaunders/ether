import { Button, SearchSelect } from "@braedonsaunders/appkit-ui";
import { useState } from "react";
import { Drawer } from "../../components/Drawer";
import { t } from "../../i18n";
import { useAsync } from "../../lib/hooks";
import { librariesApi, type Library } from "./api";

export function NarrowingEditor({
  library,
  onClose,
  onChange,
}: {
  library: Library;
  onClose: () => void;
  onChange: (collectionId: number) => void;
}) {
  const [collectionId, setCollectionId] = useState(library.collectionId ?? 0);
  const collections = useAsync(
    (signal) =>
      library.source === "collection"
        ? librariesApi.listCollectionSources(signal)
        : Promise.resolve([]),
    [library.source],
  );
  const type = String(library.mediaType ?? "movie");
  return (
    <Drawer
      open
      title={t("Collection")}
      onClose={onClose}
      size="lg"
      footer={
        <div className="flex justify-end gap-2">
          <Button variant="ghost" onClick={onClose}>
            {t("Cancel")}
          </Button>
          <Button
            disabled={library.source === "collection" && !collectionId}
            onClick={() => onChange(collectionId)}
          >
            {t("Apply")}
          </Button>
        </div>
      }
    >
      {
        <SearchSelect
          ariaLabel={t("Collection")}
          value={String(collectionId)}
          onChange={(value) => setCollectionId(Number(value))}
          options={(collections.data ?? [])
            .filter(
              (collection) => collection.mediaType === type || collection.mediaType === "mixed",
            )
            .map((collection) => ({ value: String(collection.id), label: collection.name }))}
        />
      }
    </Drawer>
  );
}
