// Copyright 2022 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>

package trie

import "github.com/ethereum/go-ethereum/common"

// ID is the identifier for uniquely identifying a trie.
// 由于状态树，合约存储树，收据树等都是用 trie 这一个数据结构构造的，所以创建一个树的时候，需要由 id 唯一标识
// 目前只针对由状态树和合约存储树
type ID struct {
	// 区块的状态的根 hash
	StateRoot common.Hash // The root of the corresponding state(block.root)
	// 当前合约存储树所属的合约地址hash
	Owner common.Hash // The contract address hash which the trie belongs to
	// trie的根hash
	Root common.Hash // The root hash of trie
}

// StateTrieID constructs an identifier for state trie with the provided state root.
// 状态树id，StateRoot == Root , Owner = 空
func StateTrieID(root common.Hash) *ID {
	return &ID{
		StateRoot: root,
		Owner:     common.Hash{},
		Root:      root,
	}
}

// StorageTrieID constructs an identifier for storage trie which belongs to a certain
// state and contract specified by the stateRoot and owner.
// 合约存储树id，需要传入块hash（stateRoot），所属合约hash（owner），以及这棵树本身的hash（root）
func StorageTrieID(stateRoot common.Hash, owner common.Hash, root common.Hash) *ID {
	return &ID{
		StateRoot: stateRoot,
		Owner:     owner,
		Root:      root,
	}
}

// TrieID constructs an identifier for a standard trie(not a second-layer trie)
// with provided root. It's mostly used in tests and some other tries like CHT trie.
func TrieID(root common.Hash) *ID {
	return &ID{
		StateRoot: root,
		Owner:     common.Hash{},
		Root:      root,
	}
}
