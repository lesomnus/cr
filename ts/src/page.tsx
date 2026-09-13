/**
 * The management side of cr: what the registry holds, who may do what to it,
 * the rules on tags, and what garbage collection did.
 *
 * Repositories, manifests and tags are the registry's rows and are only read
 * here, except a repository's description. Bindings and tag rules are written
 * here and are in force within seconds. A full collection is started through
 * `POST /admin/gc`, which a sandbox, having no registry, does not serve.
 *
 * @module
 */

import { useState } from 'react'

import { useCall, useQuery } from '@lesomnus/payday/react'
import { key } from '@lesomnus/payday/store'

import type { Binding } from '../gen/app/binding_pb.js'
import { BindingService } from '../gen/app/binding_svc_pb.js'
import type { GcRun } from '../gen/app/gc_run_pb.js'
import { GcRunService } from '../gen/app/gc_run_svc_pb.js'
import type { Manifest } from '../gen/app/manifest_pb.js'
import { ManifestService } from '../gen/app/manifest_svc_pb.js'
import type { Repository } from '../gen/app/repository_pb.js'
import { RepositoryService } from '../gen/app/repository_svc_pb.js'
import type { Tag } from '../gen/app/tag_pb.js'
import type { TagRule } from '../gen/app/tag_rule_pb.js'
import { TagRuleService } from '../gen/app/tag_rule_svc_pb.js'
import { TagService } from '../gen/app/tag_svc_pb.js'
import { TenantService } from '../gen/app/payday/tenant_svc_pb.js'

type Tab = 'repositories' | 'bindings' | 'tag rules' | 'collections'

export function Page(props: { who: string; onSignOut: () => void }): React.ReactNode {
	const [tab, setTab] = useState<Tab>('repositories')

	// `@acme/admin` -- the tenant this credential is inside, which is whose
	// bindings and tag rules a write here makes.
	const alias = props.who.replace(/^@/, '').split('/')[0] ?? ''
	const tenant = useQuery(TenantService.method.get, { ref: { key: { case: 'alias', value: alias } } })

	return (
		<main>
			<header>
				<h1>cr</h1>
				<nav>
					{(['repositories', 'bindings', 'tag rules', 'collections'] as const).map((t) => (
						<button key={t} className={t === tab ? 'selected' : undefined} onClick={() => setTab(t)}>
							{t}
						</button>
					))}
				</nav>
				<span>{props.who}</span>
				<button onClick={props.onSignOut}>sign out</button>
			</header>

			{tab === 'repositories' && <Repositories />}
			{tab === 'collections' && <Collections />}
			{(tab === 'bindings' || tab === 'tag rules') &&
				(tenant.state === 'error' ? (
					<p className="bad">{String(tenant.error)}</p>
				) : tenant.data === undefined ? (
					<p>...</p>
				) : tab === 'bindings' ? (
					<Bindings tenant={tenant.data.id} />
				) : (
					<TagRules tenant={tenant.data.id} />
				))}
		</main>
	)
}

function Failure(props: { error: unknown }): React.ReactNode {
	return <p className="bad">{String(props.error)}</p>
}

function Repositories(): React.ReactNode {
	const [selected, setSelected] = useState<string>()
	const { state, data, error } = useQuery(RepositoryService.method.list, { filters: [] })

	if (state === 'error') return <Failure error={error} />
	if (data === undefined) return <p>...</p>
	if (data.items.length === 0) return <p>no repositories yet; push one.</p>

	return (
		<section>
			<h2>repositories</h2>
			<table>
				<tbody>
					{data.items.map((v: Repository) => (
						<tr key={key(v.id)} className={v.name === selected ? 'selected' : undefined}>
							<td>
								<button onClick={() => setSelected(v.name)}>{v.name}</button>
							</td>
							<td>
								<Description repository={v} />
							</td>
						</tr>
					))}
				</tbody>
			</table>
			{selected !== undefined && <RepositoryDetail name={selected} />}
		</section>
	)
}

/** Description is the one thing about a repository this page writes. */
function Description(props: { repository: Repository }): React.ReactNode {
	const [draft, setDraft] = useState<string>()
	const patch = useCall(RepositoryService.method.patch)

	if (draft === undefined) {
		return (
			<span className="dim" onDoubleClick={() => setDraft(props.repository.desc)} title="double-click to edit">
				{props.repository.desc || '(no description)'}
			</span>
		)
	}
	return (
		<form
			onSubmit={(e) => {
				e.preventDefault()
				patch
					.call({ ref: { key: { case: 'id', value: props.repository.id } }, desc: draft })
					.then(() => setDraft(undefined))
					.catch(() => {})
			}}
		>
			<input value={draft} onChange={(e) => setDraft(e.target.value)} />
			<button type="submit" disabled={patch.state === 'pending'}>
				save
			</button>
			<button type="button" onClick={() => setDraft(undefined)}>
				cancel
			</button>
			{patch.state === 'error' && <span className="bad">{String(patch.error)}</span>}
		</form>
	)
}

function RepositoryDetail(props: { name: string }): React.ReactNode {
	const tags = useQuery(TagService.method.list, { filters: [{ repo: props.name }] })
	const manifests = useQuery(ManifestService.method.list, { filters: [{ repo: props.name }] })

	return (
		<section>
			<h3>{props.name}: tags</h3>
			{tags.state === 'error' ? (
				<Failure error={tags.error} />
			) : tags.data === undefined ? (
				<p>...</p>
			) : (
				<table>
					<tbody>
						{tags.data.items.map((v: Tag) => (
							<tr key={key(v.id)}>
								<td>{v.name}</td>
								<td className="dim">{v.digest}</td>
							</tr>
						))}
					</tbody>
				</table>
			)}

			<h3>{props.name}: manifests</h3>
			{manifests.state === 'error' ? (
				<Failure error={manifests.error} />
			) : manifests.data === undefined ? (
				<p>...</p>
			) : (
				<table>
					<tbody>
						{manifests.data.items.map((v: Manifest) => (
							<tr key={key(v.id)}>
								<td>{v.digest}</td>
								<td className="dim">{v.artifactType || v.mediaType}</td>
								<td className="dim">{String(v.size)} B</td>
								<td className="dim">{v.subject === '' ? '' : `refers to ${v.subject.slice(0, 19)}`}</td>
							</tr>
						))}
					</tbody>
				</table>
			)}
		</section>
	)
}

const actions = ['pull', 'push', 'delete', 'tag', 'catalog', 'search', 'admin', '*']

/** when reads `claim=glob` lines into a binding's conditions. */
function parseWhen(text: string): { [key: string]: string } {
	const out: { [key: string]: string } = {}
	for (const line of text.split('\n')) {
		const i = line.indexOf('=')
		if (i > 0) out[line.slice(0, i).trim()] = line.slice(i + 1).trim()
	}
	return out
}

function Bindings(props: { tenant: Uint8Array }): React.ReactNode {
	const { state, data, error } = useQuery(BindingService.method.list, { filters: [] })

	return (
		<section>
			<h2>bindings</h2>
			<p className="dim">
				Who may do what where. Bindings only add; a caller nothing matches may do nothing.
			</p>
			<AddBinding tenant={props.tenant} />
			{state === 'error' ? (
				<Failure error={error} />
			) : data === undefined ? (
				<p>...</p>
			) : (
				<table>
					<thead>
						<tr>
							<th>name</th>
							<th>who</th>
							<th>repositories</th>
							<th>actions</th>
							<th>when</th>
							<th />
						</tr>
					</thead>
					<tbody>
						{data.items.map((v: Binding) => (
							<BindingRow key={key(v.id)} binding={v} />
						))}
					</tbody>
				</table>
			)}
		</section>
	)
}

function BindingRow(props: { binding: Binding }): React.ReactNode {
	const erase = useCall(BindingService.method.erase)
	const b = props.binding
	return (
		<tr>
			<td>{b.alias}</td>
			<td>{b.subject !== '' ? b.subject : `group ${b.group}`}</td>
			<td>{b.repo}</td>
			<td>{b.actions.join(', ')}</td>
			<td className="dim">
				{Object.entries(b.when)
					.map(([k, v]) => `${k}=${v}`)
					.join(' ')}
			</td>
			<td>
				<button
					disabled={erase.state === 'pending'}
					onClick={() => erase.call({ key: { case: 'id', value: b.id } }).catch(() => {})}
				>
					erase
				</button>
			</td>
		</tr>
	)
}

function AddBinding(props: { tenant: Uint8Array }): React.ReactNode {
	const [alias, setAlias] = useState('')
	const [who, setWho] = useState('')
	const [isGroup, setIsGroup] = useState(false)
	const [repo, setRepo] = useState('')
	const [chosen, setChosen] = useState<string[]>(['pull'])
	const [when, setWhen] = useState('')
	const add = useCall(BindingService.method.add)

	return (
		<form
			className="add"
			onSubmit={(e) => {
				e.preventDefault()
				if (alias === '' || who === '' || repo === '') return
				add.call({
					tenant: { key: { case: 'id', value: props.tenant } },
					alias,
					subject: isGroup ? '' : who,
					group: isGroup ? who : '',
					repo,
					actions: chosen,
					when: parseWhen(when),
				})
					.then(() => {
						setAlias('')
						setWho('')
						setRepo('')
						setWhen('')
					})
					.catch(() => {})
			}}
		>
			<input value={alias} placeholder="name" onChange={(e) => setAlias(e.target.value)} />
			<select value={isGroup ? 'group' : 'subject'} onChange={(e) => setIsGroup(e.target.value === 'group')}>
				<option value="subject">subject</option>
				<option value="group">group</option>
			</select>
			<input value={who} placeholder={isGroup ? 'authenticated, @acme/devs' : 'anonymous, alice, @acme/ci'} onChange={(e) => setWho(e.target.value)} />
			<input value={repo} placeholder="acme/*" onChange={(e) => setRepo(e.target.value)} />
			<span>
				{actions.map((a) => (
					<label key={a}>
						<input
							type="checkbox"
							checked={chosen.includes(a)}
							onChange={(e) => setChosen(e.target.checked ? [...chosen, a] : chosen.filter((c) => c !== a))}
						/>
						{a}
					</label>
				))}
			</span>
			<textarea value={when} placeholder={'claim=glob, one per line\nworkflow_ref=acme/app/.github/workflows/release.yml@*'} onChange={(e) => setWhen(e.target.value)} />
			<button type="submit" disabled={add.state === 'pending'}>
				add
			</button>
			{add.state === 'error' && <span className="bad">{String(add.error)}</span>}
		</form>
	)
}

const kinds = ['immutable', 'protected', 'pattern', 'retention']

function TagRules(props: { tenant: Uint8Array }): React.ReactNode {
	const { state, data, error } = useQuery(TagRuleService.method.list, { filters: [] })

	return (
		<section>
			<h2>tag rules</h2>
			<AddTagRule tenant={props.tenant} />
			{state === 'error' ? (
				<Failure error={error} />
			) : data === undefined ? (
				<p>...</p>
			) : (
				<table>
					<thead>
						<tr>
							<th>name</th>
							<th>repositories</th>
							<th>tags</th>
							<th>kind</th>
							<th>with</th>
							<th />
						</tr>
					</thead>
					<tbody>
						{data.items.map((v: TagRule) => (
							<TagRuleRow key={key(v.id)} rule={v} />
						))}
					</tbody>
				</table>
			)}
		</section>
	)
}

function TagRuleRow(props: { rule: TagRule }): React.ReactNode {
	const erase = useCall(TagRuleService.method.erase)
	const r = props.rule
	const detail =
		r.kind === 'pattern' ? r.pattern : r.kind === 'retention' ? `keep ${r.keep}` : r.kind === 'protected' ? r.groups.join(', ') : ''
	return (
		<tr>
			<td>{r.alias}</td>
			<td>{r.repo}</td>
			<td>{r.tag}</td>
			<td>{r.kind}</td>
			<td className="dim">{detail}</td>
			<td>
				<button
					disabled={erase.state === 'pending'}
					onClick={() => erase.call({ key: { case: 'id', value: r.id } }).catch(() => {})}
				>
					erase
				</button>
			</td>
		</tr>
	)
}

function AddTagRule(props: { tenant: Uint8Array }): React.ReactNode {
	const [alias, setAlias] = useState('')
	const [repo, setRepo] = useState('*')
	const [tag, setTag] = useState('')
	const [kind, setKind] = useState('immutable')
	const [pattern, setPattern] = useState('')
	const [groups, setGroups] = useState('')
	const [keep, setKeep] = useState(10)
	const add = useCall(TagRuleService.method.add)

	return (
		<form
			className="add"
			onSubmit={(e) => {
				e.preventDefault()
				if (alias === '' || repo === '' || tag === '') return
				add.call({
					tenant: { key: { case: 'id', value: props.tenant } },
					alias,
					repo,
					tag,
					kind,
					pattern: kind === 'pattern' ? pattern : '',
					groups: kind === 'protected' ? groups.split(',').map((g) => g.trim()).filter((g) => g !== '') : [],
					keep: kind === 'retention' ? keep : 0,
				})
					.then(() => {
						setAlias('')
						setTag('')
					})
					.catch(() => {})
			}}
		>
			<input value={alias} placeholder="name" onChange={(e) => setAlias(e.target.value)} />
			<input value={repo} placeholder="repositories" onChange={(e) => setRepo(e.target.value)} />
			<input value={tag} placeholder="v*" onChange={(e) => setTag(e.target.value)} />
			<select value={kind} onChange={(e) => setKind(e.target.value)}>
				{kinds.map((k) => (
					<option key={k} value={k}>
						{k}
					</option>
				))}
			</select>
			{kind === 'pattern' && <input value={pattern} placeholder="v\d+\.\d+\.\d+" onChange={(e) => setPattern(e.target.value)} />}
			{kind === 'protected' && <input value={groups} placeholder="groups, comma separated" onChange={(e) => setGroups(e.target.value)} />}
			{kind === 'retention' && <input type="number" min={0} value={keep} onChange={(e) => setKeep(Number(e.target.value))} />}
			<button type="submit" disabled={add.state === 'pending'}>
				add
			</button>
			{add.state === 'error' && <span className="bad">{String(add.error)}</span>}
		</form>
	)
}

function Collections(): React.ReactNode {
	const { state, data, error } = useQuery(GcRunService.method.list, { filters: [] })
	const [started, setStarted] = useState<string>()

	return (
		<section>
			<h2>garbage collection</h2>
			<p>
				<button
					onClick={() => {
						setStarted('...')
						fetch('/admin/gc', { method: 'POST', credentials: 'include' })
							.then(async (res) => setStarted(res.ok || res.status === 409 ? `run ${(await res.json()).id}` : `refused: ${res.status}`))
							.catch((err: unknown) => setStarted(String(err)))
					}}
				>
					run a full collection
				</button>{' '}
				<span className="dim">{started}</span>
			</p>
			{state === 'error' ? (
				<Failure error={error} />
			) : data === undefined ? (
				<p>...</p>
			) : data.items.length === 0 ? (
				<p>no runs yet.</p>
			) : (
				<table>
					<thead>
						<tr>
							<th>started</th>
							<th>kind</th>
							<th>trigger</th>
							<th>state</th>
							<th>repositories</th>
							<th>tags</th>
							<th>manifests</th>
							<th>blobs</th>
							<th>freed</th>
							<th>missing</th>
						</tr>
					</thead>
					<tbody>
						{data.items.map((v: GcRun) => (
							<tr key={key(v.id)} className={v.state === 'failed' ? 'bad' : undefined} title={v.error}>
								<td>{v.dateCreated === undefined ? '' : new Date(Number(v.dateCreated.seconds) * 1000).toLocaleString()}</td>
								<td>{v.kind}</td>
								<td>{v.trigger}</td>
								<td>{v.state}</td>
								<td>{String(v.repositories)}</td>
								<td>{String(v.tags)}</td>
								<td>{String(v.manifests)}</td>
								<td>{String(v.blobs)}</td>
								<td>{bytes(Number(v.bytes))}</td>
								<td>{v.missing.length}</td>
							</tr>
						))}
					</tbody>
				</table>
			)}
		</section>
	)
}

function bytes(n: number): string {
	const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
	let i = 0
	while (n >= 1024 && i < units.length - 1) {
		n /= 1024
		i++
	}
	return `${n.toFixed(i === 0 ? 0 : 1)} ${units[i]}`
}
