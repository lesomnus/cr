/**
 * The registry as the management plane sees it: repositories, and for the one
 * picked, its tags and its manifests.
 *
 * Everything here is read through the entity services, the same rows the
 * registry writes on a push. Nothing is written from this page yet; the rows
 * are the registry's, and the API refuses to add or erase them.
 *
 * @module
 */

import { useState } from 'react'

import { useQuery } from '@lesomnus/payday/react'
import { key } from '@lesomnus/payday/store'

import type { Manifest } from '../gen/app/manifest_pb.js'
import { ManifestService } from '../gen/app/manifest_svc_pb.js'
import type { Repository } from '../gen/app/repository_pb.js'
import { RepositoryService } from '../gen/app/repository_svc_pb.js'
import type { Tag } from '../gen/app/tag_pb.js'
import { TagService } from '../gen/app/tag_svc_pb.js'

export function Page(props: { who: string; onSignOut: () => void }): React.ReactNode {
	const [repo, setRepo] = useState<string>()

	return (
		<main>
			<header>
				<h1>cr</h1>
				<span>{props.who}</span>
				<button onClick={props.onSignOut}>sign out</button>
			</header>

			<Repositories selected={repo} onSelect={setRepo} />
			{repo !== undefined && <RepositoryDetail name={repo} />}
		</main>
	)
}

function Repositories(props: { selected: string | undefined; onSelect: (name: string) => void }): React.ReactNode {
	const { state, data, error } = useQuery(RepositoryService.method.list, { filters: [] })

	if (state === 'error') return <p className="bad">{String(error)}</p>
	if (data === undefined) return <p>...</p>
	if (data.items.length === 0) return <p>no repositories yet; push one.</p>

	return (
		<section>
			<h2>repositories</h2>
			<table>
				<tbody>
					{data.items.map((v: Repository) => (
						<tr key={key(v.id)} className={v.name === props.selected ? 'selected' : undefined}>
							<td>
								<button onClick={() => props.onSelect(v.name)}>{v.name}</button>
							</td>
							<td className="dim">{v.desc}</td>
						</tr>
					))}
				</tbody>
			</table>
		</section>
	)
}

function RepositoryDetail(props: { name: string }): React.ReactNode {
	const tags = useQuery(TagService.method.list, { filters: [{ repo: props.name }] })
	const manifests = useQuery(ManifestService.method.list, { filters: [{ repo: props.name }] })

	return (
		<section>
			<h2>{props.name}</h2>

			<h3>tags</h3>
			{tags.state === 'error' ? (
				<p className="bad">{String(tags.error)}</p>
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

			<h3>manifests</h3>
			{manifests.state === 'error' ? (
				<p className="bad">{String(manifests.error)}</p>
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
