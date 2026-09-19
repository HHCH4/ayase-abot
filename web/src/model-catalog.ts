import type { Provider } from '@/types'

// 模型选择器使用不可见分隔符携带供应商上下文，保存时再还原成后端需要的模型 ID。
export const modelReferenceSeparator = '\u0000'

export interface CatalogModelOption {
  label: string
  value: string
  providerID: string
  modelID: string
}

export function makeModelReference(providerID: string, modelID: string) {
  return `${providerID}${modelReferenceSeparator}${modelID}`
}

export function parseModelReference(value: string) {
  const index = value.indexOf(modelReferenceSeparator)
  if (index < 1) return undefined
  const providerID = value.slice(0, index)
  const modelID = value.slice(index + modelReferenceSeparator.length)
  return providerID && modelID ? { providerID, modelID } : undefined
}

// configuredModelOptions 只返回已启用的模型，保证下拉框不会把停用目录重新带回运行配置。
export function configuredModelOptions(providers: Provider[]): CatalogModelOption[] {
  const options: CatalogModelOption[] = []
  for (const provider of providers) {
    for (const model of provider.models || []) {
      if (model.enabled === false || !model.id.trim()) continue
      options.push({
        label: `${provider.name} / ${model.display_name || model.id}`,
        value: makeModelReference(provider.id, model.id),
        providerID: provider.id,
        modelID: model.id,
      })
    }
  }
  return options
}
